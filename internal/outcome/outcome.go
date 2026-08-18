// Package outcome classifies a step's or job's error into the categories the
// hook system dispatches on: a task-level failure (nonzero exit / red verdict
// / a required tool that never succeeded), an infrastructure error, or an
// abort (the job's context was canceled). The pipeline collapses every step
// outcome into a single Go error; Failure marks the ones that are task-level
// failures rather than infrastructure errors, and Classify buckets the rest.
package outcome

import (
	"context"
	"errors"
)

// Class is the category a step/job outcome falls into for hook dispatch.
type Class string

const (
	// Succeeded is a nil error.
	Succeeded Class = "succeeded"
	// Failed is a task-level failure: a nonzero command exit, a fix verdict
	// still red, a required tool that never succeeded, or the step's own
	// timeout: expiring (see FailOnDeadline). Marked with Fail.
	Failed Class = "failed"
	// Errored is an infrastructure error: workspace setup, docker, an LLM
	// transport failure, template rendering, or a store write.
	Errored Class = "errored"
	// Aborted means the enclosing (job-level) context was canceled — a
	// SIGINT/SIGTERM mid-run.
	Aborted Class = "aborted"
)

// Failure marks an error as a task-level failure rather than an
// infrastructure error. It wraps, so errors.As sees through fmt.Errorf chains.
type Failure struct{ Err error }

func (f *Failure) Error() string { return f.Err.Error() }
func (f *Failure) Unwrap() error { return f.Err }

// Fail wraps err as a task-level Failure. It is nil-safe: Fail(nil) is nil.
func Fail(err error) error {
	if err == nil {
		return nil
	}

	return &Failure{Err: err}
}

// FailOnDeadline marks err a task-level Failure when attemptCtx's own
// deadline is what expired: attemptCtx is done while jobCtx is still live. A
// step that outlives its timeout: is the step saying no — it was given a
// budget and did not finish inside it — which is also Concourse's call (a
// timed-out step is failed there, so on_failure fires). Errored stays
// reserved for the machinery breaking.
//
// It is the classification twin of retry.StopOnDeadline and shares its
// contract: each caller owns the attempt context it passes, and a deadline
// from *inside* an attempt (an MCP or HTTP client's own timeout) does not
// trip it. A nil err, a live attemptCtx, or a canceled jobCtx (an abort)
// all pass err through untouched.
func FailOnDeadline(jobCtx, attemptCtx context.Context, err error) error {
	if err == nil || attemptCtx.Err() == nil || jobCtx.Err() != nil {
		return err
	}

	return Fail(err)
}

// Process exit codes, one per outcome class a caller can act on.
const (
	// ExitOK is a successful run.
	ExitOK = 0
	// ExitFailed is a task-level failure: something the pipeline ran said no.
	ExitFailed = 1
	// ExitErrored is an infrastructure or configuration error: the pipeline
	// could not be run as written. Worth retrying only after a fix, where a
	// Failed run may just mean "the tests are red".
	ExitErrored = 2
	// ExitAborted is the conventional 128+SIGINT for a canceled run.
	ExitAborted = 130
)

// ExitCode maps a top-level error to the process's exit status, so a script
// wrapping steps can tell "my task failed" from "docker is down" from "I hit
// Ctrl-C" — all three of which used to exit 1.
//
// It is Classify's counterpart at the process boundary, where there is no job
// context to consult: an abort is detected from the error chain instead. A
// config or CLI error carries no Failure marker and so lands on ExitErrored,
// which is right — an unparseable pipeline is not a red test.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}

	if errors.Is(err, context.Canceled) {
		return ExitAborted
	}

	var failure *Failure
	if errors.As(err, &failure) {
		return ExitFailed
	}

	return ExitErrored
}

// Classify buckets err for hook dispatch. ctx must be the enclosing
// (job-level) context: an abort is "the run was canceled", so it is tested via
// ctx.Err() first — keying on errors.Is(err, context.DeadlineExceeded) instead
// would misclassify an internal per-step timeout (e.g. an agent step's own
// WithTimeout) as an abort while the job context is still live.
func Classify(ctx context.Context, err error) Class {
	if err == nil {
		return Succeeded
	}

	if ctx.Err() != nil {
		return Aborted
	}

	var failure *Failure
	if errors.As(err, &failure) {
		return Failed
	}

	return Errored
}
