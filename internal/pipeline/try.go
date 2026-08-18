package pipeline

// try: — run a step, tolerate its failure or error; only an abort propagates.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/outcome"
)

// runTryStep executes a try: wrapper. It runs the inner step exactly as if it
// were unwrapped — through runNonGetStep, so the inner step's own when: guard,
// hooks and execution log all behave normally — and hands its REAL outcome
// back to the plan walker.
//
// Nothing is swallowed here. Toleration is deliberately the last thing that
// happens to the error, in planWalk.runStep, after routing: that is what lets
// a `to: {failure: ...}` on the wrapper fire, and what keeps an aborted inner
// step from being reported as a green job. An earlier revision called
// dispatchNonGetStep and returned nil from here, which cost both at once.
func runTryStep(ctx context.Context, r stepRunner, i int, step config.Step, parentHash string) (stepResult, error) {
	content, err := merkle.TryNodeContent(r.cfg, step)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (try): %w", i, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindTry, content, parentHash)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (try): %w", i, err)
	}

	inner := *step.Try
	name := executedStepName(inner)

	fmt.Printf("try: %s\n", name)
	slog.Debug("job.step", "job", r.jobName, "index", i, "kind", "try", "inner", name)

	// The inner step chains under the try node's hash. No caching (nil
	// skippable): try is always unskippable, so the inner step always runs.
	res, innerErr := runNonGetStep(ctx, r, i, inner, nil, hash)

	// The wrapper's node status is what the plan did with the outcome, not
	// what the inner step's outcome was (the inner step records that itself):
	// "succeeded" when the outcome is about to be tolerated, "failed" when it
	// is the one class try: does not cover (abort) and the job stops here.
	status := "succeeded"
	if innerErr != nil && !toleratedByTry(ctx, innerErr) {
		status = "failed"
	}

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindTry, StepIndex: i, Resource: name, Content: content}
	_ = r.st.RecordNode(context.WithoutCancel(ctx), nodeRecord(node), r.jobName, status, nil, innerErr)

	res.hash = hash

	return res, innerErr
}

// toleratedByTry reports whether err is the kind of outcome a try: wrapper
// exists to swallow: a task-level failure or an infrastructure error —
// Concourse's line (its TryStep masks failures, errors and timeouts alike,
// source @ v8.2.4).
//
// An Aborted (job-ctx-canceled) step is NOT tolerated, also per Concourse:
// swallowing it would report a green job for a Ctrl-C and march the plan
// into steps whose context is already dead. Note this is a wider net than
// to: routing casts — to: routes only success/failure, so an errored step
// never routes but IS tolerated here.
func toleratedByTry(ctx context.Context, err error) bool {
	return err != nil && outcome.Classify(ctx, err) != outcome.Aborted
}

// tolerateTryFailure is a try: wrapper's whole effect on the plan: it turns the
// inner step's failure or infrastructure error into a nil error so the walk
// continues, and says so on the transcript. It runs AFTER applyRouting, so a
// wrapper that routed on the failure has already consumed the error and prints
// nothing extra here. Any non-try step, and an abort, passes through.
func tolerateTryFailure(ctx context.Context, jobName string, step config.Step, err error) error {
	if err == nil || step.Try == nil || !toleratedByTry(ctx, err) {
		return err
	}

	name := executedStepName(step)

	fmt.Printf("try: %s %s (tried, continuing)\n", name, outcome.Classify(ctx, err))
	slog.Info("job.try", "job", jobName, "step", name, "outcome", "tolerated", "error", err.Error())

	return nil
}
