// Package pipeline orchestrates a job's plan: resolving/fetching get steps,
// running task/put/agent steps in order, and recording each step's outcome
// so later runs can skip unchanged work (see internal/merkle).
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/redact"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// bestEffort logs a store write that must not mask the error being returned; a lost FinishRun leaves a run 'running' and a lost ForgetChain leaves a skip entry for failed work.
func bestEffort(ctx context.Context, operation string, err error) {
	if err != nil {
		slog.ErrorContext(ctx, "store.best_effort_failed", "operation", operation, "error", err)
	}
}

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
	// stepResumed: a previous attempt of this run already finished the step, so it is not run again; parentHash advances to the node that attempt left.
	stepResumed
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
	// verdict is the agent's verdict, or for a replayed pass (stepResumed) the routing key that pass recorded, success and failure included.
	verdict string
	note    string
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
//
// The content is masked against the run's secrets, as everything the run
// prints is: a hidden load_var: value rendered into a command is in the
// content its hash was computed from, and the store is read by every page.
func nodeRecord(ctx context.Context, n merkle.Node) store.NodeRecord {
	return store.NodeRecord{
		Hash:       n.Hash,
		ParentHash: n.ParentHash,
		Kind:       string(n.Kind),
		StepIndex:  n.StepIndex,
		Resource:   n.Resource,
		Content:    maskedContent(redact.SecretsFrom(ctx), n.Content),
	}
}

// maskedContent is content with every string in it masked. Through JSON, the
// form the store keeps it in, so a string inside any type a builder used is
// reached; content that does not encode is the store's to refuse, as before.
func maskedContent(secrets *redact.Secrets, content map[string]any) map[string]any {
	if secrets.Empty() {
		return content
	}

	raw, err := json.Marshal(content)
	if err != nil {
		return content
	}

	var generic any

	err = json.Unmarshal(raw, &generic)
	if err != nil {
		return content
	}

	masked, _ := maskValue(secrets, generic).(map[string]any)

	return masked
}

// maskValue is value with every string in it masked, copied rather than masked in place.
func maskValue(secrets *redact.Secrets, value any) any {
	switch typed := value.(type) {
	case string:
		return secrets.String(typed)
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, nested := range typed {
			out[key] = maskValue(secrets, nested)
		}

		return out
	case []any:
		out := make([]any, len(typed))
		for i, nested := range typed {
			out[i] = maskValue(secrets, nested)
		}

		return out
	default:
		return value
	}
}

// recordStepFailure records a step's failed node, classifying the outcome
// (failed vs errored vs aborted), and drops the chain from the skip index —
// a --force rerun that fails must not skip on the older success. Both write
// under a detached context so an aborted step's outcome still persists rather
// than being dropped by the canceled context. Best-effort: recording errors are logged, never returned, so they can't
// mask the original error returned to the caller.
func recordStepFailure(ctx context.Context, r stepRunner, node merkle.Node, err error) {
	recordStepFailureWith(ctx, r, node, nil, err)
}

// recordStepFailureWith is recordStepFailure for a step whose failure still
// produced something worth keeping: a failed agent step is exactly the one
// somebody reconstructs afterwards, so its response and tool calls are
// recorded rather than dropped.
func recordStepFailureWith(ctx context.Context, r stepRunner, node merkle.Node, result map[string]any, err error) {
	status := store.NodeStatus(outcome.Classify(ctx, err))
	recCtx := context.WithoutCancel(ctx)
	bestEffort(ctx, "RecordNode", r.st.RecordNode(recCtx, nodeRecord(ctx, node), r.jobName, status, result, err))
	bestEffort(ctx, "ForgetChain", r.st.ForgetChain(recCtx, r.jobName, node.Hash))
}

// recordBlock records a container's own node under the class its outcome
// earned — the same failed/errored/aborted line a leaf's row draws, so an
// aborted block is not stored as a failure. Best-effort and detached, like
// recordStepFailure.
func recordBlock(ctx context.Context, r stepRunner, node merkle.Node, result map[string]any, err error) {
	recordBlockAs(ctx, r, node, outcome.Classify(ctx, err), result, err)
}

func recordBlockAs(ctx context.Context, r stepRunner, node merkle.Node, class outcome.Class, result map[string]any, err error) {
	if class == outcome.Succeeded {
		bestEffort(ctx, "RecordNode", recordSucceeded(context.WithoutCancel(ctx), r, node, result))

		return
	}

	bestEffort(ctx, "RecordNode", r.st.RecordNode(context.WithoutCancel(ctx), nodeRecord(ctx, node), r.jobName, store.NodeStatus(class), result, err))
}

type ownNodeKey struct{}

// ownNode is what the step runNonGetStep is running recorded as its own success.
type ownNode struct {
	record store.NodeRecord
	result map[string]any
}

// withOwnNode scopes ctx to one step's own node record, which nested steps shadow with theirs.
func withOwnNode(ctx context.Context) (context.Context, *ownNode) {
	own := &ownNode{}

	return context.WithValue(ctx, ownNodeKey{}, own), own
}

// recordSucceeded records node succeeded and keeps it as the step's own, for a hook that fails the step afterwards.
func recordSucceeded(ctx context.Context, r stepRunner, node merkle.Node, result map[string]any) error {
	record := nodeRecord(ctx, node)

	if own, _ := ctx.Value(ownNodeKey{}).(*ownNode); own != nil {
		own.record, own.result = record, result
	}

	return r.st.RecordNode(ctx, record, r.jobName, store.NodeSucceeded, result, nil) //nolint:wrapcheck // each caller names the step
}

// failedBy re-records the step's own node under the outcome a hook gave it: left succeeded, every reader of the node, the matrix cache's HasNodeSucceeded among them, calls a failed step green.
func (own *ownNode) failedBy(ctx context.Context, r stepRunner, err error) {
	if own.record.Hash == "" {
		return
	}

	bestEffort(ctx, "RecordNode", r.st.RecordNode(context.WithoutCancel(ctx), own.record, r.jobName, store.NodeStatus(outcome.Classify(ctx, err)), own.result, err))
}

// runPlaced owns a leaf step's recording order: the node, then where it ran — run_placements references the node, and a step that FAILED on a worker is the one whose machine somebody wants named.
func runPlaced(
	ctx context.Context, r stepRunner, node merkle.Node, placedAs string, work func(context.Context) (map[string]any, error),
) (stepResult, error) {
	ctx, placed := withPlacementSink(ctx)

	result, err := work(ctx)
	if err != nil {
		recordStepFailure(ctx, r, node, err)
		recordPlacement(ctx, r, placed, node.StepIndex, placedStep{name: placedAs, slot: node.Hash, node: node.Hash})

		return failedAt(node.Hash), err
	}

	err = recordSucceeded(ctx, r, node, result)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (%s %q): could not record node %q: %w", node.StepIndex, node.Kind, placedAs, node.Hash, err)
	}

	recordPlacement(ctx, r, placed, node.StepIndex, placedStep{name: placedAs, slot: node.Hash, node: node.Hash})

	return ran(node.Hash), nil
}

// noResult adapts a step whose node records no result to runPlaced.
func noResult(work func(context.Context) error) func(context.Context) (map[string]any, error) {
	return func(ctx context.Context) (map[string]any, error) {
		return nil, work(ctx)
	}
}
