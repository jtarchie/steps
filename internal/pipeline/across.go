package pipeline

// The across: modifier — one step, once per combination of values.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
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
func runAcrossStep(ctx context.Context, r stepRunner, i int, step config.Step, parentHash string) (stepResult, error) {
	label := fmt.Sprintf("job %q step %d", r.jobName, i)

	// A from_file: axis takes its values from a file an earlier step wrote, so
	// the matrix's width is only knowable here — see acrossfile.go.
	fromFile, err := resolveFileAxes(ctx, label, step, r.bw)
	if err != nil {
		return stepResult{}, err
	}

	cells, err := config.ExpandAcrossValues(label, step, fromFile)
	if err != nil {
		return stepResult{}, err //nolint:wrapcheck // ExpandAcrossValues already carries the job/step label
	}

	content, err := merkle.AcrossNodeContent(r.cfg, step, cells)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (across): %w", i, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindAcross, content, parentHash)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (across): %w", i, err)
	}

	reportCellCount(r.jobName, i, step, len(cells))

	// A collecting block owns its artifact wholesale: reset it to an empty
	// directory before any cell captures. That is what keeps a stale
	// same-named artifact — an earlier step's, or a wider expansion's from an
	// earlier visit of this block — from silently merging into this run's
	// collection, and what makes the artifact exist even when zero cells
	// survive, so the consumer walks what survived (possibly nothing) rather
	// than failing to materialize a missing input.
	err = resetCollectedArtifacts(ctx, label, step, r.bw)
	if err != nil {
		return stepResult{}, err
	}

	// Cells are parented on the block's OWN parent, not on the block hash.
	// The block hash folds in every cell's content, so parenting cells under
	// it would make one cell's edit change every cell's identity — which is
	// precisely the whole-matrix re-run this feature exists to avoid.
	cellErr := runAcrossCells(ctx, r, i, step, cells, parentHash)

	status := "succeeded"
	if cellErr != nil {
		status = "failed"
	}

	node := merkle.Node{
		Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindAcross,
		StepIndex: i, Resource: executedStepName(step), Content: content,
	}
	_ = r.st.RecordNode(context.WithoutCancel(ctx), nodeRecord(node), r.jobName, status, nil, cellErr)

	return ran(hash), cellErr
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
	kind := cellBlockKind(step)

	if cells > 0 {
		fmt.Printf("%s: %d cells\n", kind, cells)
		slog.Debug("job.step", "job", jobName, "index", i, "kind", kind, "cells", cells)

		return
	}

	fmt.Printf("across: 0 cells (%s is empty); nothing to run\n", emptyAxisSource(step))
	slog.Warn("across.empty", "job", jobName, "index", i, "source", emptyAxisSource(step))
}

// cellBlockKind names a matrix block the way its author wrote it: the
// desugared Parallelism field is the one trace that this across: began as
// parallelism:, and the run report speaks that word back. The zero-cell
// branch above stays across-only — a parallelism: matrix is static, so it is
// never empty.
func cellBlockKind(step config.Step) string {
	if step.Parallelism > 0 {
		return "parallelism"
	}

	return "across"
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
	ctx context.Context, r stepRunner, i int, step config.Step, cells []config.Step, cellParent string,
) error {
	if step.MaxInFlight > 1 {
		return runAcrossCellsConcurrently(ctx, r, i, step, cells, cellParent)
	}

	var failures []error

	cellCtx, spend := newBlockBudget(ctx, r.cfg, step, cells)

	for index, cell := range cells {
		if stopAdmitting(ctx, r.jobName, spend, index, len(cells)) {
			break
		}

		skipped, err := runAcrossCell(cellCtx, r, i, cell, cellParent)

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
	ctx context.Context, r stepRunner, i int, step config.Step, cells []config.Step, cellParent string,
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

	cellCtx, spend := newBlockBudget(ctx, r.cfg, step, cells)
	spend.warnIfUnbindable(r.jobName, step.MaxInFlight, len(cells))

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

		if stopAdmitting(ctx, r.jobName, spend, index, len(cells)) {
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
			runCtx, cellLog := forkExecLog(cellCtx)
			logs[index] = cellLog

			skipped, err := runAcrossCell(runCtx, r, i, cells[index], cellParent)
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

// runAcrossCell runs one cell unless its exact content already succeeded.
func runAcrossCell(ctx context.Context, r stepRunner, i int, cell config.Step, cellParent string) (bool, error) {
	cellHash, cacheable, err := merkle.CellHash(r.cfg, cell, cellParent)
	if err != nil {
		return false, err //nolint:wrapcheck // CellHash names the cell
	}

	if cacheable && !forced(ctx) {
		done, lookupErr := r.st.HasNodeSucceeded(ctx, r.jobName, cellHash)
		if lookupErr != nil {
			return false, fmt.Errorf("cell cache: %w", lookupErr)
		}

		if done {
			return true, nil
		}
	}

	_, err = runNonGetStep(ctx, r, i, cell, nil, cellParent)

	return false, err
}
