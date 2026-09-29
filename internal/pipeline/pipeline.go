// Package pipeline orchestrates a job's plan: resolving/fetching get steps,
// running task/put/agent steps in order, and recording each step's outcome
// so later runs can skip unchanged work (see internal/merkle).
package pipeline

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// stepRunner is what every step runner needs regardless of kind: the pipeline
// to resolve against, the job it belongs to, the workspace it runs in, and the
// store it records to. A triggered build swaps the workspace with withBuild.
type stepRunner struct {
	cfg     *config.Config
	jobName string
	bw      workspace.BuildWorkspace
	st      store.Store
}

func (r stepRunner) withBuild(bw workspace.BuildWorkspace) stepRunner {
	r.bw = bw

	return r
}

// scope is the hook-dispatch view of this runner, labelled for logging.
func (r stepRunner) scope(label string) hookScope {
	return hookScope{stepRunner: r, label: label}
}

// stepDisposition is what happened to one non-get step, distinguishing the
// two very different reasons a step might not have executed.
type stepDisposition int

const (
	// stepRan: the step executed; advance parentHash to its node.
	stepRan stepDisposition = iota
	// stepGuardSkipped: the step's when: guard was false. Only THIS step is
	// skipped — the plan continues with the next one, and parentHash does not
	// advance (no node was produced).
	stepGuardSkipped
	// stepChainSkipped: the step's hash matched an already-succeeded chain, so
	// everything downstream of it also already succeeded. The whole remaining
	// plan is skipped.
	stepChainSkipped
	// stepCacheHit: this step's declared outputs were restored from an earlier
	// run that did the same work over the same input bytes. Unlike a chain
	// skip, only THIS step is skipped — the plan continues, and parentHash
	// advances to the step's node exactly as if it had run, because as far as
	// everything downstream can observe, it did.
	stepCacheHit
	// stepResumeKept: a resumed get whose build already got past it, so the
	// artifact is kept as the skipped steps left it. The plan continues
	// under the get's node, as if it had fetched.
	stepResumeKept
)

// stepResult is what running one step produced: the node hash the next step
// chains under, what happened to it, and — for an agent step — the verdict
// applyRouting keys on plus its note. A zero hash ("nothing to chain under")
// is the right answer on every error path.
type stepResult struct {
	hash string
	// nodeHash is the step's OWN node, the only hash its row is published
	// under. It differs from hash wherever the walk chains on something else:
	// a failure (nothing — a to: route carries on under the parent), a chain
	// skip or a get that fetched nothing (the parent), a guard skip (the
	// parent, and no node at all). Kept apart so no path can publish a
	// parent's node as a step's own.
	nodeHash    string
	disposition stepDisposition
	verdict     string
	note        string
	// stepID is the display-tree id the step ran under, for a caller that says something about it after it returned (a tolerated try:).
	stepID int64
}

// ran is the ordinary outcome: the step executed and produced hash.
func ran(hash string) stepResult {
	return stepResult{hash: hash, nodeHash: hash}
}

// failedAt is a step that failed after recording its node under hash.
func failedAt(hash string) stepResult {
	return stepResult{nodeHash: hash}
}

// settled is a block that recorded its node under hash, then ended with err:
// a failed block names its node but chains nothing, so a to: route carries
// the walk on under the block's parent.
func settled(hash string, err error) stepResult {
	if err != nil {
		return failedAt(hash)
	}

	return ran(hash)
}

// published is the hash a step's row is shown under: its own node, never a
// parent it passed through, which would compare the row against a different
// step and hand it that step's spend and machine.
func (r stepResult) published() string {
	return r.nodeHash
}

// nodeRecord converts a plan merkle.Node into the shape store.RecordNode
// persists, keeping the store package free of a dependency on merkle's Node
// type.
func nodeRecord(n merkle.Node) store.NodeRecord {
	return store.NodeRecord{
		Hash:       n.Hash,
		ParentHash: n.ParentHash,
		Kind:       string(n.Kind),
		StepIndex:  n.StepIndex,
		Resource:   n.Resource,
		Content:    n.Content,
	}
}

// recordStepFailure records a step's failed node, classifying the outcome
// (failed vs errored vs aborted), and drops the chain from the skip index —
// a --force rerun that fails must not skip on the older success. Both write
// under a detached context so an aborted step's outcome still persists rather
// than being dropped by the canceled context. Best-effort: recording errors are ignored so they can't
// mask the original error returned to the caller.
func recordStepFailure(ctx context.Context, r stepRunner, node merkle.Node, err error) {
	status := string(outcome.Classify(ctx, err))
	recCtx := context.WithoutCancel(ctx)
	_ = r.st.RecordNode(recCtx, nodeRecord(node), r.jobName, status, nil, err)
	_ = r.st.ForgetChain(recCtx, r.jobName, node.Hash)
}

// runPlaced owns a leaf step's recording order: the node, then where it ran — run_placements references the node, and a step that FAILED on a worker is the one whose machine somebody wants named.
func runPlaced(
	ctx context.Context, r stepRunner, node merkle.Node, placedAs string, work func(context.Context) (map[string]any, error),
) (stepResult, error) {
	ctx, placed := withPlacementSink(ctx)

	result, err := work(ctx)
	if err != nil {
		recordStepFailure(ctx, r, node, err)
		recordPlacement(ctx, r, placed, node.StepIndex, placedAs, node.Hash, node.Hash)

		return failedAt(node.Hash), err
	}

	err = r.st.RecordNode(ctx, nodeRecord(node), r.jobName, "succeeded", result, nil)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (%s %q): could not record node %q: %w", node.StepIndex, node.Kind, placedAs, node.Hash, err)
	}

	recordPlacement(ctx, r, placed, node.StepIndex, placedAs, node.Hash, node.Hash)

	return ran(node.Hash), nil
}

// noResult adapts a step whose node records no result to runPlaced.
func noResult(work func(context.Context) error) func(context.Context) (map[string]any, error) {
	return func(ctx context.Context) (map[string]any, error) {
		return nil, work(ctx)
	}
}
