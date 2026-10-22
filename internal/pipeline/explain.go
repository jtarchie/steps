package pipeline

// Answering "what would this run actually do?" without doing it.

import (
	"context"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/store"
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
func Explain(ctx context.Context, cfg *config.Config, job *config.Job, pinned map[string]string, st explainStore) ([]ExplainRow, error) {
	err := validateJob(ctx, cfg, job)
	if err != nil {
		return nil, err
	}

	ctx, releaseWorkers := withJobLeases(ctx)
	defer func() { releaseWorkers(ctx) }()

	// RunJob's sequence minus what a run does with it: no refresh (a plan
	// reports from what is recorded), no cursor lock and no take (a plan takes
	// nothing), and no narrowing to one build (a plan lists every waiting set,
	// each of which some later build runs).
	inputs, err := resolveJobInputs(ctx, cfg, st, job, pinned, false)
	if err != nil {
		return nil, err
	}

	chains, skippable, err := planSkippable(ctx, cfg, st, job, pinned, inputs.cache, inputs.resolution)
	if err != nil {
		return nil, err
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
		// A chain holds one node per plan step, in order, so the position is the plan index; node.StepIndex restarts at zero after every get.
		for index, node := range chain.Nodes {
			if seen[node.Hash] {
				continue
			}

			seen[node.Hash] = true

			rows = append(rows, ExplainRow{
				StepIndex: index,
				Kind:      string(node.Kind),
				Name:      node.Resource,
				ShortHash: shortHash(node.Hash),
				WouldSkip: skippable[node.Hash],
				Reason:    explainReason(cfg, job, index, skippable[node.Hash], chain.Unskippable),
			})
		}
	}

	return rows
}

// explainReason says why a node would run or skip, reusing the same
// vocabulary the run itself prints (see merkle.UncacheableReason).
func explainReason(cfg *config.Config, job *config.Job, index int, wouldSkip, chainUnskippable bool) string {
	if wouldSkip {
		return "cached"
	}

	// A step with nothing against it of its own still never caches when
	// something else on its chain cannot; "not yet run" read as a job that had
	// simply not been tried, however often it had.
	notCached := "not yet run"
	if chainUnskippable {
		notCached = "chain uncacheable"
	}

	if index < 0 || index >= len(job.Plan) {
		return notCached
	}

	step := job.Plan[index]

	reason, err := merkle.UncacheableReason(cfg, step)
	if err == nil && reason != "" {
		return reason
	}

	return notCached
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

// explainStore is what a plan reads: recorded versions and the skip index.
type explainStore interface {
	store.Versions
	store.Cache
}
