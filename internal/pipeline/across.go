package pipeline

// The across: modifier — one step, once per combination of values.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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
	runtime, err := resolveRuntimeAxes(ctx, st, agent.RunIDFrom(ctx), step)
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
	cellErr := runAcrossCells(ctx, cfg, jobName, i, cells, bw, st, parentHash, handoff)

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

// runAcrossCells runs each cell in declaration order, skipping any that has
// already succeeded with identical content.
//
// In order rather than concurrently: cells commonly share a workspace, and the
// value of a matrix is mostly in not hand-maintaining the copies. An
// in_parallel: inside a cell covers the case where a cell's own work should
// overlap.
func runAcrossCells(
	ctx context.Context, cfg *config.Config, jobName string, i int, cells []config.Step,
	bw workspace.BuildWorkspace, st *store.Store, cellParent string, handoff *agent.Handoff,
) error {
	var failures []error

	for _, cell := range cells {
		skipped, err := runAcrossCell(ctx, cfg, jobName, i, cell, bw, st, cellParent, handoff)
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
