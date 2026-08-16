// Package pipeline orchestrates a job's plan: resolving/fetching get steps,
// running task/put/agent steps in order, and recording each step's outcome
// so later runs can skip unchanged work (see internal/merkle).
package pipeline

import (
	"context"

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
	st      *store.Store
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
)

// stepResult is what running one step produced: the node hash the next step
// chains under, what happened to it, and — for an agent step — the verdict
// applyRouting keys on plus its note. The zero value ("nothing ran, no
// verdict") is the right answer on every error path.
type stepResult struct {
	hash        string
	disposition stepDisposition
	verdict     string
	note        string
}

// ran is the ordinary outcome: the step executed and produced hash.
func ran(hash string) stepResult {
	return stepResult{hash: hash}
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

// recordStepFailure records a step's failed node and job_run, classifying the
// outcome (failed vs errored vs aborted) and writing under a detached context
// so an aborted step's outcome still persists rather than being dropped by the
// canceled context. Best-effort: recording errors are ignored so they can't
// mask the original error returned to the caller.
func recordStepFailure(ctx context.Context, r stepRunner, node merkle.Node, err error) {
	status := string(outcome.Classify(ctx, err))
	recCtx := context.WithoutCancel(ctx)
	_ = r.st.RecordNode(recCtx, nodeRecord(node), r.jobName, status, nil, err)
	_ = r.st.RecordJobRun(recCtx, r.jobName, node.Hash, status, err)
}
