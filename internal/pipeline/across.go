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

	for index, cell := range cells {
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

	for index := range cells {
		// Acquired in the parent, before the goroutine starts, so cells are
		// ADMITTED in declaration order — under a limit especially, "which
		// cells go first" is otherwise whichever goroutines the scheduler
		// happened to run, which is nothing a pipeline author can reason about.
		slot.acquire()

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
