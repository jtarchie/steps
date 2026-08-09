package pipeline

// The job-level wall-clock deadline (`timeout:` on a job).
//
// A job whose width is decided at run time has no bound on how long it runs:
// timeout: is per step, per attempt, so twelve cells that each finish just
// inside their own deadline is a job with no deadline at all. This is the
// other unit of the ceiling budget: already provides — one bounds the spend,
// this one bounds the wall clock.
//
// It is a plain deadline VALUE rather than a context.WithTimeout, and that is
// the whole design. Cancelling the context would cut off whatever step happened
// to be running, racing the step's own timeout: and reporting a job-level
// deadline breach for work that was still making progress. Instead the plan
// walk asks, between steps, whether there is time to start another — so the two
// timeouts compose, and the cost is that a job may overrun by at most one
// step's duration. That is the honest price of not interrupting work.

import (
	"context"
	"fmt"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/outcome"
)

type jobDeadlineKey struct{}

// jobDeadline is the wall-clock ceiling for one job run.
type jobDeadline struct {
	at      time.Time
	timeout time.Duration
}

// withJobDeadline scopes a job's wall-clock ceiling to its run. A job with no
// timeout: installs nothing, so every pipeline that does not use this pays no
// attention to it at all.
func withJobDeadline(ctx context.Context, job *config.Job) context.Context {
	if job.Timeout == "" {
		return ctx
	}

	timeout, err := config.ParseTimeout(job.Timeout)
	if err != nil || timeout <= 0 {
		// Unreachable: validateJobTimeouts rejects both at load. Ignoring it
		// here rather than failing the run keeps a malformed value from being
		// enforced as an instant expiry.
		return ctx
	}

	return context.WithValue(ctx, jobDeadlineKey{}, jobDeadline{at: time.Now().Add(timeout), timeout: timeout})
}

// jobDeadlinePassed reports whether the job's wall-clock ceiling has been
// reached, with the error to fail the job by.
//
// A job-level failure, the same class as exceeding max_visits: an operator set
// a bound and the run crossed it. So it reaches the job's own on_failure and
// ensure hooks, which is where a "this took too long" notification belongs.
func jobDeadlinePassed(ctx context.Context, jobName string) error {
	deadline, ok := ctx.Value(jobDeadlineKey{}).(jobDeadline)
	if !ok {
		return nil
	}

	elapsed := time.Since(deadline.at) + deadline.timeout
	if elapsed < deadline.timeout {
		return nil
	}

	//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
	return outcome.Fail(fmt.Errorf(
		"job %q exceeded its timeout of %s (%s elapsed); the running step was allowed to finish, and no further step was started",
		jobName, deadline.timeout, elapsed.Round(time.Second)))
}
