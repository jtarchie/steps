package pipeline

import (
	"context"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/retry"
)

// retryWithTimeout runs fn up to attempts times (attempts < 1 is treated as
// 1), giving each attempt its own context bounded by timeoutStr when that
// parses to a positive duration — per-attempt, not a single deadline shared
// across the retries. On a retry (the second attempt onward) it calls marker
// with the 1-based attempt number and the total so the caller can print its
// own progress line. It is the single retry+per-attempt-timeout scaffold every
// get/task/put step shares; a per-attempt timeout expires only the attempt's
// context, leaving the parent ctx (which governs retry.Do's backoff and abort)
// untouched, so a job abort stays distinguishable from a step overrunning its
// own budget. An overrun ends the step immediately (retry.Stop) rather than
// spending the remaining attempts re-failing against the same budget.
func retryWithTimeout(ctx context.Context, attempts int, timeoutStr string, marker func(attempt, total int), fn func(ctx context.Context) error) error {
	timeout, err := config.ParseTimeout(timeoutStr)
	if err != nil {
		return err //nolint:wrapcheck // caller wraps with its own step context
	}

	total := max(attempts, 1)

	return retry.Do(ctx, total, func(attempt int) error { //nolint:wrapcheck // every caller wraps this func's own return with its step context
		if attempt > 0 && marker != nil {
			marker(attempt+1, total)
		}

		attemptCtx := ctx

		if timeout > 0 {
			var cancel context.CancelFunc

			attemptCtx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}

		// On this step's own wall clock expiring, stop: the same work against
		// the same budget would just expire again.
		return retry.StopOnDeadline(ctx, attemptCtx, fn(attemptCtx))
	})
}
