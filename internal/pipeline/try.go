package pipeline

// try: — run a step, tolerate its task-level failure.

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
// a `to: {failure: ...}` on the wrapper fire, and what keeps an aborted or
// infrastructure-errored inner step from being reported as a green job. An
// earlier revision called dispatchNonGetStep and returned nil from here, which
// cost all three at once.
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
	// "succeeded" when the failure is about to be tolerated, "failed" when it
	// is one of the classes try: does not cover and the job stops here.
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
// exists to swallow: a task-level failure and nothing else.
//
// An Errored (infrastructure) or Aborted (ctx-canceled) step is NOT tolerated.
// This is the same line outcomeKey draws for to: routing, for the same reason:
// swallowing them would report a green job for a Ctrl-C or a docker outage,
// and would let the plan march on into steps whose context is already dead.
func toleratedByTry(ctx context.Context, err error) bool {
	return err != nil && outcome.Classify(ctx, err) == outcome.Failed
}

// tolerateTryFailure is a try: wrapper's whole effect on the plan: it turns the
// inner step's task-level failure into a nil error so the walk continues, and
// says so on the transcript. It runs AFTER applyRouting, so a wrapper that
// routed on the failure has already consumed the error and prints nothing extra
// here. Any non-try step, and any outcome try: doesn't cover, passes through.
func tolerateTryFailure(ctx context.Context, jobName string, step config.Step, err error) error {
	if step.Try == nil || !toleratedByTry(ctx, err) {
		return err
	}

	name := executedStepName(step)

	fmt.Printf("try: %s failed (tried, continuing)\n", name)
	slog.Info("job.try", "job", jobName, "step", name, "outcome", "tolerated", "error", err.Error())

	return nil
}
