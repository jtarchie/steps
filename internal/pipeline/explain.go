package pipeline

// Answering "what would this run actually do?" without doing it.

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// ExplainRow is one planned step and what a run would do with it.
type ExplainRow struct {
	StepIndex int
	Kind      string
	Name      string
	ShortHash string
	WouldSkip bool
	Reason    string
}

// Explain reports, per planned node, whether a run would execute or skip it.
//
// The planner already computes exactly this and then immediately acts on it,
// so the answer existed and was simply never offered: the only way to find out
// what a run would skip was to start one and read the transcript. That is a
// poor trade when the question is usually "is my cache in the state I think it
// is?" — which is precisely when you don't want to run anything.
//
// It resolves get versions the way planning always has (check commands do
// run), but executes no step and writes no node, job_run, or workspace.
func Explain(ctx context.Context, cfg *config.Config, job *config.Job, pinned map[string]string, st *store.Store) ([]ExplainRow, error) {
	err := workspace.ValidateArtifactFlow(cfg, job)
	if err != nil {
		return nil, fmt.Errorf("job %q: %w", job.Name, err)
	}

	// Same cursor a run would use, so `steps plan` does not list versions a
	// version: every fan-out has already taken and would not run.
	cursor, err := loadVersionCursor(ctx, st, job, true)
	if err != nil {
		return nil, fmt.Errorf("job %q: %w", job.Name, err)
	}

	// And the same check cursor, so the checks `steps plan` runs ask the same
	// question a run's would.
	checked, err := loadLastChecked(ctx, st)
	if err != nil {
		return nil, fmt.Errorf("job %q: %w", job.Name, err)
	}

	chains, err := merkle.PlanChains(ctx, cfg, job.Name, job.Plan, pinned,
		rsrc.NewCache(rsrc.WithConsumed(cursor.has), rsrc.WithLastChecked(checked.get)))
	if err != nil {
		return nil, fmt.Errorf("job %q: planning: %w", job.Name, err)
	}

	skippable, err := buildSkippableIndex(ctx, st, job.Name, chains)
	if err != nil {
		return nil, fmt.Errorf("job %q: %w", job.Name, err)
	}

	return explainRows(cfg, job, chains, skippable), nil
}

// explainRows flattens the planned chains into one row per distinct node,
// in plan order. A get with version: every fans out into several chains that
// share their leading nodes, so the same hash can appear more than once.
func explainRows(cfg *config.Config, job *config.Job, chains []merkle.Chain, skippable map[string]bool) []ExplainRow {
	seen := make(map[string]bool)

	var rows []ExplainRow

	for _, chain := range chains {
		for _, node := range chain.Nodes {
			if seen[node.Hash] {
				continue
			}

			seen[node.Hash] = true

			rows = append(rows, ExplainRow{
				StepIndex: node.StepIndex,
				Kind:      string(node.Kind),
				Name:      node.Resource,
				ShortHash: shortHash(node.Hash),
				WouldSkip: skippable[node.Hash],
				Reason:    explainReason(cfg, job, node, skippable[node.Hash]),
			})
		}
	}

	return rows
}

// explainReason says why a node would run or skip, reusing the same
// vocabulary the run itself prints (see unskippableReason).
func explainReason(cfg *config.Config, job *config.Job, node merkle.Node, wouldSkip bool) string {
	if wouldSkip {
		return "cached"
	}

	if node.StepIndex < 0 || node.StepIndex >= len(job.Plan) {
		return "not yet run"
	}

	step := job.Plan[node.StepIndex]

	reason := unskippableReason(step)
	if reason != "" {
		return reason
	}

	unskippable, err := stepForcesUnskippable(cfg, step)
	if err == nil && unskippable {
		return "fix: agent"
	}

	return "not yet run"
}

// shortHash abbreviates a node hash to the leading digits, which is all a
// human comparing two plans needs.
func shortHash(hash string) string {
	const width = 12
	if len(hash) <= width {
		return hash
	}

	return hash[:width]
}
