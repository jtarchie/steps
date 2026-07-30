// Package retry provides a linear-backoff retry loop.
package retry

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// agentRetryBackoffUnit is the linear-backoff step between attempts. It's
// deliberately coarse: these attempts wrap a network round trip to an LLM
// endpoint (rate limiting, transient 5xx), where a short pause is more
// likely to help than to waste time.
const agentRetryBackoffUnit = 500 * time.Millisecond

// stopError marks an error as not worth retrying. It wraps, so errors.As sees
// through fmt.Errorf chains — the same shape as outcome.Failure.
type stopError struct{ Err error }

func (s *stopError) Error() string { return s.Err.Error() }
func (s *stopError) Unwrap() error { return s.Err }

// Stop wraps err so Do returns it immediately instead of spending the
// remaining attempts on it. It is nil-safe: Stop(nil) is nil.
//
// The caller decides what is unretryable, because only it knows why the
// attempt failed. The case this exists for is a per-attempt timeout expiring:
// attempts: retries a transient fault, but it cannot buy more time, so a
// second attempt against the same budget just re-fails deterministically and
// bills twice for it.
//
// Do unwraps the marker before returning, so it never escapes this package:
// callers' errors.As/errors.Is checks and the final error message see the
// original chain unchanged.
func Stop(err error) error {
	if err == nil {
		return nil
	}

	return &stopError{Err: err}
}

// StopOnDeadline marks err unretryable when attemptCtx's own deadline is what
// expired: attemptCtx is done while the enclosing ctx is still live. It is the
// one place the "a timeout is not transient" rule lives, shared by all three
// retry scaffolds (retryWithTimeout in internal/pipeline, runPrepared and
// RunFix in internal/agent), each of which owns the attempt context it passes.
//
// Testing the context rather than errors.Is(err, context.DeadlineExceeded) is
// deliberate: a deadline from *inside* an attempt — an MCP or HTTP client's own
// timeout — is an ordinary transient fault and must stay retryable. Only the
// step's own timeout: ends the loop.
//
// A nil err, a live attemptCtx, or a canceled ctx (a job abort, which Do
// handles on its own path) all pass err through untouched.
func StopOnDeadline(ctx, attemptCtx context.Context, err error) error {
	if err == nil || attemptCtx.Err() == nil || ctx.Err() != nil {
		return err
	}

	return Stop(err)
}

// Do calls fn up to attempts times (attempts < 1 is treated as 1),
// stopping at the first success. Between attempts it sleeps
// attempt*agentRetryBackoffUnit, growing the pause linearly with each
// retry. ctx cancellation aborts immediately. An error fn wrapped with Stop
// ends the loop at once (see Stop). The last error is returned if every
// attempt fails.
func Do(ctx context.Context, attempts int, fn func(attempt int) error) error {
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error

	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err() //nolint:wrapcheck // ctx.Err() is a well-known sentinel (context.Canceled/DeadlineExceeded), wrapping adds no information
			case <-time.After(time.Duration(attempt) * agentRetryBackoffUnit):
			}
		}

		lastErr = fn(attempt)
		if lastErr == nil {
			return nil
		}

		var stop *stopError
		if errors.As(lastErr, &stop) {
			slog.Warn("retry.not_retryable", "attempt", attempt+1, "attempts", attempts, "error", stop.Err)

			return stop.Err
		}

		slog.Warn("retry.attempt_failed", "attempt", attempt+1, "attempts", attempts, "error", lastErr)
	}

	return lastErr
}
