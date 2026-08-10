package pipeline

// The across: modifier — one step, once per combination of values.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// runAcrossStep runs one step once per combination of its across: values.
//
// Without it a matrix is written out by hand — one near-identical step per
// combination, all of which have to be kept in sync, so adding one Go version
// means editing every block.
//
// The cells are siblings rather than a sequence, and each is cached
// individually against its own node hash. That is the headline advantage over
// Concourse, which re-runs the entire matrix on any change: here, changing one
// value in one axis re-runs only the cells that value appears in.
func runAcrossStep(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step,
	bw workspace.BuildWorkspace, st *store.Store, parentHash string, handoff *agent.Handoff,
) (string, stepDisposition, nonGetOutcome, error) {
	label := fmt.Sprintf("job %q step %d", jobName, i)

	// A from: axis takes its values from what an earlier step recorded, so the
	// matrix's width is only knowable here — see acrossruntime.go.
	runtime, err := resolveRuntimeAxes(ctx, st, agent.ContextReadScopes(ctx), step)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("%s: %w", label, err)
	}

	cells, err := config.ExpandAcrossValues(label, step, runtime)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, err //nolint:wrapcheck // ExpandAcrossValues already carries the job/step label
	}

	content, err := merkle.AcrossNodeContent(cfg, step, cells)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (across): %w", i, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindAcross, content, parentHash)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (across): %w", i, err)
	}

	fmt.Printf("across: %d cells\n", len(cells))
	slog.Debug("job.step", "job", jobName, "index", i, "kind", "across", "cells", len(cells))

	// Cells are parented on the block's OWN parent, not on the block hash.
	// The block hash folds in every cell's content, so parenting cells under
	// it would make one cell's edit change every cell's identity — which is
	// precisely the whole-matrix re-run this feature exists to avoid.
	cellErr := runAcrossCells(ctx, cfg, jobName, i, step, cells, bw, st, parentHash, handoff)

	status := "succeeded"
	if cellErr != nil {
		status = "failed"
	}

	node := merkle.Node{
		Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindAcross,
		StepIndex: i, Resource: executedStepName(step), Content: content,
	}
	_ = st.RecordNode(context.WithoutCancel(ctx), nodeRecord(node), jobName, status, nil, cellErr)

	return hash, stepRan, nonGetOutcome{}, cellErr
}

// runAcrossCells runs each cell, skipping any that has already succeeded with
// identical content.
//
// Serial in declaration order by default: cells commonly share a workspace, and
// the value of a matrix is mostly in not hand-maintaining the copies. A step
// that says max_in_flight: gets the concurrent walk below instead — a runtime
// fan-out over N independent agent cells has no ordering between them, and
// serializing it costs N times one cell's wall clock for nothing.
func runAcrossCells(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, cells []config.Step,
	bw workspace.BuildWorkspace, st *store.Store, cellParent string, handoff *agent.Handoff,
) error {
	if step.MaxInFlight > 1 {
		return runAcrossCellsConcurrently(ctx, cfg, jobName, i, step, cells, bw, st, cellParent, handoff)
	}

	// context qualify: is a property of the MATRIX, not of the scheduling, so
	// a serial qualified matrix scopes and merges exactly as the concurrent
	// walk does. That is what makes the two spellings of one pipeline record
	// the same key names — the reason max_in_flight: is safe to add and remove.
	qualified := step.Unwrap().QualifiesContext()

	var (
		failures []error
		results  []branchResult // only the qualified walk has a join to merge
	)

	if qualified {
		results = make([]branchResult, len(cells))
	}

	spend := newBlockBudget(ctx, step)

	for index, cell := range cells {
		if stopAdmitting(ctx, jobName, spend, index, len(cells)) {
			break
		}

		cellCtx := ctx

		if qualified {
			results[index] = branchResult{index: index, name: executedStepName(cell)}
			cellCtx = agent.WithContextScope(ctx,
				branchContextScope(agent.ContextWriteScope(ctx), index, results[index].name))
		}

		skipped, err := runAcrossCell(cellCtx, cfg, jobName, i, cell, bw, st, cellParent, handoff)
		if err != nil {
			failures = append(failures, fmt.Errorf("cell %q: %w", executedStepName(cell), err))

			// A failing cell does not stop the rest. A matrix is asking "which
			// of these combinations work", and stopping at the first red one
			// answers that question for exactly one cell.
			continue
		}

		if skipped {
			fmt.Printf("skip: %s (unchanged)\n", executedStepName(cell))
		}
	}

	if qualified {
		mergeBranchesContext(ctx, st, results)
	}

	return errors.Join(failures...)
}

// runAcrossCellsConcurrently runs up to max_in_flight cells at a time.
//
// It borrows in_parallel:'s machinery wholesale — the same limiter, the same
// per-branch execution log merged in declaration order, the same per-branch
// context scopes merged at the join — because a cell running beside its
// siblings has exactly the hazards a branch does. What it deliberately does NOT
// borrow is fail_fast: a matrix asks which combinations work, and cancelling
// the siblings of the first red cell answers that for exactly one cell. Every
// cell runs, every failure is reported, same as the serial walk.
func runAcrossCellsConcurrently(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, cells []config.Step,
	bw workspace.BuildWorkspace, st *store.Store, cellParent string, handoff *agent.Handoff,
) error {
	var (
		wg      sync.WaitGroup
		slot    = newLimiter(step.MaxInFlight, len(cells))
		logs    = make([]*execLog, len(cells))
		results = make([]branchResult, len(cells))
		skips   = make([]bool, len(cells))
	)

	for index := range cells {
		results[index] = branchResult{index: index, name: executedStepName(cells[index])}
	}

	spend := newBlockBudget(ctx, step)
	spend.warnIfUnbindable(jobName, step.MaxInFlight, len(cells))

	for index := range cells {
		// Checked in the PARENT, before a slot is taken, so the block stops
		// admitting new cells while the ones already in flight run to
		// completion and keep what they recorded. Checking inside the goroutine
		// would instead race every cell that had already been admitted.
		// Acquired in the parent, before the goroutine starts, so cells are
		// ADMITTED in declaration order — under a limit especially, "which
		// cells go first" is otherwise whichever goroutines the scheduler
		// happened to run, which is nothing a pipeline author can reason about.
		//
		// BEFORE the ceiling check, not after. Acquiring blocks until a cell
		// finishes and rolls its usage up, so the check that follows reads
		// spend that includes it. Checked first instead, every admission until
		// the limit saturates sees a total of ~0 — the cells it is deciding
		// against are still running — so a matrix could launch max_in_flight
		// cells before the ceiling meant anything at all.
		slot.acquire()

		if stopAdmitting(ctx, jobName, spend, index, len(cells)) {
			slot.release()

			break
		}

		wg.Add(1)

		go func() {
			defer wg.Done()
			defer slot.release()

			// Cells finish in whatever order they finish, so recording as they
			// go would make assert.execution nondeterministic. Each records
			// into its own log, merged below in declaration order.
			runCtx, cellLog := forkExecLog(ctx)
			logs[index] = cellLog

			// Each cell writes context into a scope only it touches, merged at
			// the join under a key naming the cell. Serial cells keep the
			// plain last-wins the docs describe, because sequential writers
			// resolve in an order readable off the pipeline; concurrent ones do
			// not, which is the same reason in_parallel: branches are scoped.
			runCtx = agent.WithContextScope(runCtx,
				branchContextScope(agent.ContextWriteScope(ctx), index, results[index].name))

			skipped, err := runAcrossCell(runCtx, cfg, jobName, i, cells[index], bw, st, cellParent, handoff)
			results[index].err, skips[index] = err, skipped
		}()
	}

	wg.Wait()

	var failures []error

	for _, result := range results {
		if logs[result.index] != nil {
			mergeExecLog(ctx, logs[result.index])
		}

		switch {
		case result.err != nil:
			failures = append(failures, fmt.Errorf("cell %q: %w", result.name, result.err))
		case skips[result.index]:
			fmt.Printf("skip: %s (unchanged)\n", result.name)
		}
	}

	mergeBranchesContext(ctx, st, results)

	return errors.Join(failures...)
}

// blockBudget is an across: block's token allowance: what its cells may spend
// TOGETHER, and how much of it is left.
//
// Measured as a delta against the job's own accumulator rather than a counter
// of its own — every agent step already rolls its provider-reported usage up
// into RunUsage, so the matrix's spend is simply what the job's total moved by
// while the block ran. No new plumbing, and it counts a cell's retries and
// sub-agents for free, since those roll up the same way.
//
// A zero budget (the common case) is a nil ceiling that never trips.
type blockBudget struct {
	usage   *agent.RunUsage
	start   int
	ceiling int
}

func newBlockBudget(ctx context.Context, step config.Step) *blockBudget {
	ceiling := stepBudgetTokens(step)
	if ceiling <= 0 {
		return &blockBudget{}
	}

	usage := agent.RunUsageFrom(ctx)
	if usage == nil {
		// No accumulator means no agent step can report anything, so there is
		// nothing to meter. Not an error: a matrix of tasks with a budget is
		// pointless, not wrong.
		return &blockBudget{}
	}

	return &blockBudget{usage: usage, start: usage.Total(), ceiling: ceiling}
}

// exhausted reports whether the block has spent its allowance.
//
// Checked BEFORE a cell is started, never mid-cell: a cell that has begun runs
// to completion and keeps what it recorded. That is the whole difference
// between this and the job ceiling — the job's is a backstop and fails, this
// one stops handing out new work.
func (b *blockBudget) exhausted() bool {
	return b.usage != nil && b.spent() >= b.ceiling
}

// warnIfUnbindable says so when this block's width makes its own ceiling
// decorative.
//
// An admission-time ceiling can only see what FINISHED cells spent, and a cell
// only finishes once a slot is contended for. So when max_in_flight is at or
// above the cell count there is no serialization point anywhere in the block:
// newLimiter hands out a limiter that never blocks, every cell is admitted
// against a total of ~0, and the budget bounds precisely nothing. That is
// inherent to bounding what gets STARTED rather than what a running cell may
// cost — not something a different check order fixes — so the honest move is
// to be loud about it rather than let an author believe a ceiling is in force.
//
// The matrix's width is usually decided at run time (from:), which is why this
// cannot be a load-time error: whether the configuration binds is not knowable
// until the cells exist.
func (b *blockBudget) warnIfUnbindable(jobName string, maxInFlight, cells int) {
	if !b.unbindable(maxInFlight, cells) {
		return
	}

	fmt.Printf("budget: warning — max_in_flight (%d) covers all %d cells, so this block's budget of %s tokens cannot stop anything\n",
		maxInFlight, cells, humanCount(b.ceiling))

	slog.Warn("across.budget.unbindable",
		"job", jobName, "max_in_flight", maxInFlight, "cells", cells, "budget_tokens", b.ceiling,
		"detail", "every cell is admitted before any has reported usage; lower max_in_flight, or rely on the job budget as the backstop")
}

// unbindable reports whether this block's own ceiling can stop nothing: it has
// one, and the width covers every cell, so no admission ever reads a nonzero
// total. Mirrors newLimiter's own "limit >= branches means no limiter at all".
func (b *blockBudget) unbindable(maxInFlight, cells int) bool {
	return b.usage != nil && maxInFlight >= cells
}

func (b *blockBudget) spent() int {
	if b.usage == nil {
		return 0
	}

	return b.usage.Total() - b.start
}

// report announces that the matrix stopped early. Loudly, because a truncated
// fan-out that says nothing reads exactly like a complete one that found less:
// the cells that never ran recorded nothing, so their silence is
// indistinguishable from a clean result unless this says otherwise.
func (b *blockBudget) report(jobName string, ran, total int) {
	fmt.Printf("budget: across stopped after %d of %d cells (spent %s of %s tokens)\n",
		ran, total, humanCount(b.spent()), humanCount(b.ceiling))

	slog.Warn("across.budget.exhausted",
		"job", jobName, "cells_run", ran, "cells_total", total,
		"spent_tokens", b.spent(), "budget_tokens", b.ceiling)
}

// stopAdmitting reports whether this matrix should start no further cell:
// either it has spent its own allowance, or the JOB's wall-clock deadline has
// passed (see deadlineStopsFanOut, which in_parallel: shares).
func stopAdmitting(ctx context.Context, jobName string, spend *blockBudget, ran, total int) bool {
	if spend.exhausted() {
		spend.report(jobName, ran, total)

		return true
	}

	return deadlineStopsFanOut(ctx, jobName, "across", "cells", ran, total)
}

// runAcrossCell runs one cell unless its exact content already succeeded.
func runAcrossCell(
	ctx context.Context, cfg *config.Config, jobName string, i int, cell config.Step,
	bw workspace.BuildWorkspace, st *store.Store, cellParent string, handoff *agent.Handoff,
) (bool, error) {
	cellHash, cacheable, err := merkle.CellHash(cfg, cell, cellParent)
	if err != nil {
		return false, err //nolint:wrapcheck // CellHash names the cell
	}

	if cacheable && !forced(ctx) {
		done, lookupErr := st.HasNodeSucceeded(ctx, jobName, cellHash)
		if lookupErr != nil {
			return false, fmt.Errorf("cell cache: %w", lookupErr)
		}

		if done {
			return true, nil
		}
	}

	_, _, _, err = runNonGetStep(ctx, cfg, jobName, i, cell, bw, st, nil, cellParent, handoff)

	return false, err
}
