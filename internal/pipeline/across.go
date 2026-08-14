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
	bw workspace.BuildWorkspace, st *store.Store, parentHash string,
) (string, stepDisposition, nonGetOutcome, error) {
	label := fmt.Sprintf("job %q step %d", jobName, i)

	// A from_file: axis takes its values from a file an earlier step wrote, so
	// the matrix's width is only knowable here — see acrossfile.go.
	fromFile, err := resolveFileAxes(ctx, label, step, bw)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, err
	}

	cells, err := config.ExpandAcrossValues(label, step, fromFile)
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

	reportCellCount(jobName, i, step, len(cells))

	// A collecting block owns its artifact wholesale: reset it to an empty
	// directory before any cell captures. That is what keeps a stale
	// same-named artifact — an earlier step's, or a wider expansion's from an
	// earlier visit of this block — from silently merging into this run's
	// collection, and what makes the artifact exist even when zero cells
	// survive, so the consumer walks what survived (possibly nothing) rather
	// than failing to materialize a missing input.
	err = resetCollectedArtifacts(ctx, label, step, bw)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, err
	}

	// Cells are parented on the block's OWN parent, not on the block hash.
	// The block hash folds in every cell's content, so parenting cells under
	// it would make one cell's edit change every cell's identity — which is
	// precisely the whole-matrix re-run this feature exists to avoid.
	cellErr := runAcrossCells(ctx, cfg, jobName, i, step, cells, bw, st, parentHash)

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

// resetCollectedArtifacts replaces each artifact a collecting matrix captures
// into with a fresh empty directory — see the call site in runAcrossStep for
// why. A no-op for a matrix that does not collect.
func resetCollectedArtifacts(ctx context.Context, label string, step config.Step, bw workspace.BuildWorkspace) error {
	artifacts := config.CollectedArtifacts(step)
	if len(artifacts) == 0 {
		return nil
	}

	resetter, ok := bw.(workspace.ArtifactResetter)
	if !ok {
		// Unreachable through LoadConfig: a collecting matrix requires an
		// isolating workspace, and the isolating build implements the reset.
		// A build that doesn't is a wiring bug, not a user error.
		return fmt.Errorf("%s: collected outputs need a workspace that can reset artifacts; %T cannot", label, bw)
	}

	for _, name := range artifacts {
		err := resetter.ResetArtifact(ctx, name)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}

	return nil
}

// reportCellCount announces how wide the matrix turned out to be.
//
// A matrix that expands to NOTHING says so, naming the file it read: a
// from_file: axis over an empty array is a legitimate success ("the scan found
// nothing"), and the plan carries on — but a block that ran no cells and a
// block whose cells all passed are otherwise the same silence. An author who
// wants an empty list to fail asserts it where the file is written, which is
// the step that knows what empty means.
func reportCellCount(jobName string, i int, step config.Step, cells int) {
	if cells > 0 {
		fmt.Printf("across: %d cells\n", cells)
		slog.Debug("job.step", "job", jobName, "index", i, "kind", "across", "cells", cells)

		return
	}

	fmt.Printf("across: 0 cells (%s is empty); nothing to run\n", emptyAxisSource(step))
	slog.Warn("across.empty", "job", jobName, "index", i, "source", emptyAxisSource(step))
}

// emptyAxisSource names what a zero-cell matrix read, for the line above. The
// first file axis, since a static axis cannot be empty (load-time error) — so
// an empty product is always some file's doing.
func emptyAxisSource(step config.Step) string {
	for _, axis := range step.Across {
		if axis.Runtime() {
			return axis.FromFile
		}
	}

	return "the matrix"
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
	bw workspace.BuildWorkspace, st *store.Store, cellParent string,
) error {
	if step.MaxInFlight > 1 {
		return runAcrossCellsConcurrently(ctx, cfg, jobName, i, step, cells, bw, st, cellParent)
	}

	var failures []error

	spend := newBlockBudget(ctx, cfg, step, cells)

	for index, cell := range cells {
		if stopAdmitting(ctx, jobName, spend, index, len(cells)) {
			break
		}

		skipped, err := runAcrossCell(ctx, cfg, jobName, i, cell, bw, st, cellParent)

		// Settled here rather than deferred: the serial walk holds one cell at
		// a time, and its spend is in the accumulator the moment this returns.
		spend.settle()

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

	return errors.Join(failures...)
}

// runAcrossCellsConcurrently runs up to max_in_flight cells at a time.
//
// It borrows in_parallel:'s machinery wholesale — the same limiter, the same
// per-branch execution log merged in declaration order — because a cell
// running beside its siblings has exactly the hazards a branch does. What it
// deliberately does NOT borrow is fail_fast: a matrix asks which combinations
// work, and cancelling the siblings of the first red cell answers that for
// exactly one cell. Every cell runs, every failure is reported, same as the
// serial walk.
func runAcrossCellsConcurrently(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, cells []config.Step,
	bw workspace.BuildWorkspace, st *store.Store, cellParent string,
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

	spend := newBlockBudget(ctx, cfg, step, cells)
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
			// Runs before the slot is released (defers are LIFO), so the next
			// cell admitted in the parent reads a settled reservation and this
			// cell's real spend rather than both at once.
			defer spend.settle()

			// Cells finish in whatever order they finish, so recording as they
			// go would make assert.execution nondeterministic. Each records
			// into its own log, merged below in declaration order.
			runCtx, cellLog := forkExecLog(ctx)
			logs[index] = cellLog

			skipped, err := runAcrossCell(runCtx, cfg, jobName, i, cells[index], bw, st, cellParent)
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
//
// Admission RESERVES against the ceiling rather than only reading what
// finished cells reported. spent() alone can see a cell's cost only after it
// ends, so the first max_in_flight cells were always admitted against a total
// of ~0 — and at a width covering every cell there is no serialization point
// anywhere in the block, so the ceiling bounded nothing at all. Reserving on
// admit and settling on completion makes overshoot a function of how wrong
// the reservation is, instead of how many cells got a free pass.
type blockBudget struct {
	usage   *agent.RunUsage
	start   int
	ceiling int
	// reserve is what one unfinished cell is assumed to cost; 0 means no
	// reservation source was found, which reduces admission to exactly the
	// spent()-only rule this had before.
	reserve int

	// mu guards reserved alone. spent() reads through RunUsage, which has its
	// own lock; reserved is written by finishing CELL goroutines and read by
	// the admitting parent, so it needs one of its own.
	mu       sync.Mutex
	reserved int
}

func newBlockBudget(ctx context.Context, cfg *config.Config, step config.Step, cells []config.Step) *blockBudget {
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

	return &blockBudget{
		usage: usage, start: usage.Total(), ceiling: ceiling,
		reserve: cellReserve(cfg, step, cells),
	}
}

// cellReserve is what admission assumes one unfinished cell will spend, in
// precedence order: the block's own reserve_per_cell:, then the cell agent's
// budget.tokens (the author already declared what one invocation may cost),
// then nothing — which leaves the block admitting exactly as it did before
// reservations existed, warning included.
//
// The block's own field wins, which inverts the order issue #62 proposed. Its
// way round, an agent with a budget.tokens would silently shadow a
// reserve_per_cell: written right there on the block — config that reads like
// it binds something and does not, which is the shape this codebase rejects
// at load everywhere else. The explicit, more local declaration wins instead,
// and the common case the issue is really describing (an agent budget, no
// reserve_per_cell:) is unaffected either way.
//
// Resolved ONCE for the block rather than per cell: agent: is not among the
// fields a matrix renders per cell (see config.renderableFields), so every
// cell of a block resolves to the same agent and the same ceiling.
//
// A CLI agent yields nothing here. Its ceiling is budget.usd, which meters
// dollars inside its own subprocess and cannot be compared against a token
// allowance — so such a block needs an explicit reserve_per_cell: to bind.
func cellReserve(cfg *config.Config, step config.Step, cells []config.Step) int {
	if step.Budget != nil && step.Budget.ReservePerCell > 0 {
		return step.Budget.ReservePerCell
	}

	if len(cells) == 0 {
		return 0
	}

	// A try: cell keeps the agent on the step it wraps, not on the wrapper.
	body := cells[0]
	if body.Try != nil {
		body = *body.Try
	}

	if body.Agent == "" {
		return 0
	}

	ri, err := cfg.ResolveAgentInvocation(body)
	if err != nil {
		// An unresolvable agent is a run-time failure the cell itself will
		// report far more clearly; refusing to reserve here just leaves the
		// block admitting as it always did.
		return 0
	}

	return ri.BudgetTokens
}

// admit reports whether another cell may start, taking its reservation when
// it may.
//
// Checked BEFORE a cell is started, never mid-cell: a cell that has begun runs
// to completion and keeps what it recorded. That is the whole difference
// between this and the job ceiling — the job's is a backstop and fails, this
// one stops handing out new work.
//
// Refusing takes no reservation, so a refused cell cannot hold allowance the
// cells still running might come in under.
func (b *blockBudget) admit() bool {
	if b.usage == nil {
		return true
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.spent()+b.reserved >= b.ceiling {
		return false
	}

	b.reserved += b.reserve

	return true
}

// settle releases a finished cell's reservation. Its real spend is already in
// the job accumulator by now — stepUsage.finish rolls it up before the
// conversation returns — so spent() picks it up with no double count. Calling
// this any earlier would open a window where neither the reservation nor the
// actual spend is counted, and a cell could be admitted against it.
func (b *blockBudget) settle() {
	if b.usage == nil {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.reserved -= b.reserve
}

// warnIfUnbindable says so when this block's width makes its own ceiling
// decorative.
//
// A reservation does not automatically fix that, which is the trap worth
// being loud about: at a width covering every cell NOTHING reports before the
// last cell is admitted, so the whole decision runs on reservations, and the
// most the ceiling ever sees is what the other cells hold. Reserve the
// allowance divided by the cell count and every cell is admitted by
// construction, whatever they go on to spend — a budget that reads like a
// ceiling and is arithmetically incapable of refusing anyone.
func (b *blockBudget) warnIfUnbindable(jobName string, maxInFlight, cells int) {
	if !b.unbindable(maxInFlight, cells) {
		return
	}

	fmt.Printf("budget: warning — max_in_flight (%d) covers all %d cells and %s reserved per cell cannot reach the %s ceiling, so this block's budget cannot stop anything\n",
		maxInFlight, cells, humanCount(b.reserve), humanCount(b.ceiling))

	slog.Warn("across.budget.unbindable",
		"job", jobName, "max_in_flight", maxInFlight, "cells", cells,
		"budget_tokens", b.ceiling, "reserve_per_cell", b.reserve,
		"detail", "every cell is admitted before any has reported usage; raise budget.reserve_per_cell above the allowance divided by the cell count, lower max_in_flight so real spend gates later cells, or rely on the job budget as the backstop")
}

// unbindable reports whether this block's own ceiling can stop nothing.
//
// Two conditions together. The width has to cover every cell — otherwise a
// slot is contended for, a cell finishes and reports, and admission decides on
// real spend (newLimiter's own "limit >= branches means no limiter at all").
// And the reservations have to be too small to reach the ceiling on their own:
// with no cell reporting, the largest total any admission can see is what the
// other cells hold, so the ceiling can refuse someone only when
// (cells-1) * reserve reaches it. A zero reservation is the degenerate case of
// that same test, and is why this reported true before reservations existed.
func (b *blockBudget) unbindable(maxInFlight, cells int) bool {
	if b.usage == nil || cells <= 0 || maxInFlight < cells {
		return false
	}

	return (cells-1)*b.reserve < b.ceiling
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
//
// It reports what admission actually decided on — spend PLUS the reservations
// still standing — not spend alone. Under max_in_flight the cells that
// consumed the allowance are typically still running and have reported
// nothing, so "spent 0 of 3,600,000" would name the one number that had no
// part in the decision and read like a stop that never should have happened.
func (b *blockBudget) report(jobName string, ran, total int) {
	spent, reserved := b.committed()

	fmt.Printf("budget: across stopped after %d of %d cells (%s of %s tokens committed: %s spent, %s reserved by cells still running)\n",
		ran, total, humanCount(spent+reserved), humanCount(b.ceiling), humanCount(spent), humanCount(reserved))

	slog.Warn("across.budget.exhausted",
		"job", jobName, "cells_run", ran, "cells_total", total,
		"spent_tokens", spent, "reserved_tokens", reserved, "budget_tokens", b.ceiling)
}

// committed is what admission weighed: reported spend, and the allowance held
// by cells that have not reported yet.
func (b *blockBudget) committed() (spent, reserved int) {
	if b.usage == nil {
		return 0, 0
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	return b.spent(), b.reserved
}

// stopAdmitting reports whether this matrix should start no further cell:
// either the JOB's wall-clock deadline has passed (see deadlineStopsFanOut,
// which in_parallel: shares), or the block cannot afford another cell.
//
// The deadline is checked FIRST so a deadline stop takes no reservation:
// admit() consumes allowance as its way of saying yes, and a cell refused for
// time would otherwise leave a reservation behind that nothing ever settles.
func stopAdmitting(ctx context.Context, jobName string, spend *blockBudget, ran, total int) bool {
	if deadlineStopsFanOut(ctx, jobName, "across", "cells", ran, total) {
		return true
	}

	if !spend.admit() {
		spend.report(jobName, ran, total)

		return true
	}

	return false
}

// runAcrossCell runs one cell unless its exact content already succeeded.
func runAcrossCell(
	ctx context.Context, cfg *config.Config, jobName string, i int, cell config.Step,
	bw workspace.BuildWorkspace, st *store.Store, cellParent string,
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

	_, _, _, err = runNonGetStep(ctx, cfg, jobName, i, cell, bw, st, nil, cellParent)

	return false, err
}
