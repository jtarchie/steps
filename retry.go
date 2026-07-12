package main

import (
	"context"
	"log/slog"
	"time"
)

// agentRetryBackoffUnit is the linear-backoff step between attempts. It's
// much coarser than enableWAL's 5ms unit (store.go) because these attempts
// wrap a network round trip to an LLM endpoint, not a local sqlite pragma —
// a short pause is more likely to matter (rate limiting, transient 5xx)
// than to just waste time.
const agentRetryBackoffUnit = 500 * time.Millisecond

// withRetry calls fn up to attempts times (attempts < 1 is treated as 1),
// stopping at the first success. Between attempts it sleeps
// attempt*agentRetryBackoffUnit, mirroring enableWAL's backoff shape. ctx
// cancellation aborts immediately. The last error is returned if every
// attempt fails.
func withRetry(ctx context.Context, attempts int, fn func(attempt int) error) error {
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

		slog.Warn("retry.attempt_failed", "attempt", attempt+1, "attempts", attempts, "error", lastErr)
	}

	return lastErr
}
