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
	// still red, or a required tool that never succeeded. Marked with Fail.
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
