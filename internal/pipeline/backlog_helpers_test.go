package pipeline

import (
	"context"
	"errors"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// drainJob runs job the way the daemon walks a version: every backlog — one
// run per waiting set, each queued by the one before it — and joins what they
// returned.
func drainJob(
	ctx context.Context, cfg *config.Config, job *config.Job, pinned map[string]string,
	provider workspace.Provider, st store.Store,
) error {
	var errs []error

	for {
		queued := false
		runCtx := WithBacklog(ctx, func(context.Context, int) { queued = true })

		err := RunJob(runCtx, cfg, job, pinned, provider, st, false)
		if err != nil {
			errs = append(errs, err)
		}

		if !queued {
			return errors.Join(errs...)
		}
	}
}
