package web

// Making the UI able to act, not only report.
//
// A trigger from the web goes into the same durable queue `steps watch` uses,
// and is drained by the same pipeline.RunJob every other path calls. There is
// deliberately no second execution route: a job started from a browser must
// be the same job, with the same caching, hooks, serial groups, and recording,
// as one started from a terminal.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/workspace"
)

// drainIdleBackoff is how long a drainer waits after finding nothing to do.
const drainIdleBackoff = time.Second

// LocalRunner drains each pipeline's queue in this process. It is the Runner
// the `steps web` command installs; a read-only server has none.
type LocalRunner struct {
	providers map[string]workspace.Provider
	// forced remembers which queued rows were requested with force, since the
	// queue row itself has no such column. Keyed by queue id, consumed once.
	//
	// In memory rather than in the schema because a force is a property of
	// this request, not of the job: a forced row that outlived a restart and
	// then re-ran everything would be a surprise nobody asked for. Losing the
	// flag on restart degrades to an ordinary (cached) run, which is the safe
	// direction.
	mu     sync.Mutex
	forced map[string]bool
}

// NewLocalRunner builds a runner over per-pipeline workspace providers, keyed
// by pipeline slug.
func NewLocalRunner(providers map[string]workspace.Provider) *LocalRunner {
	return &LocalRunner{providers: providers, forced: map[string]bool{}}
}

// Enqueue puts a job on the pipeline's queue.
func (r *LocalRunner) Enqueue(ctx context.Context, target *Pipeline, jobName, reason string, force bool) (int64, error) {
	err := target.Store.EnqueueJob(ctx, jobName, reason)
	if err != nil {
		return 0, fmt.Errorf("web: %w", err)
	}

	if !force {
		return 0, nil
	}

	// The row id is not returned by EnqueueJob, so mark the job itself: the
	// next claim of this job consumes the flag. Two forced requests for one
	// job collapse into one forced run, which is what a person double-clicking
	// "re-run" meant anyway.
	r.mu.Lock()
	r.forced[jobKey(target.Slug, jobName)] = true
	r.mu.Unlock()

	return 0, nil
}

// jobKey identifies a job within a pipeline. A plain string, not a hash: two
// job names colliding would consume the wrong force flag, re-running one job
// from scratch while the job actually asked for quietly runs cached.
func jobKey(slug, jobName string) string {
	return slug + "\x00" + jobName
}

// takeForce consumes a pending force flag for a job.
func (r *LocalRunner) takeForce(slug, jobName string) bool {
	key := jobKey(slug, jobName)

	r.mu.Lock()
	defer r.mu.Unlock()

	forced := r.forced[key]
	delete(r.forced, key)

	return forced
}

// Drain runs each pipeline's queue until ctx is canceled. One goroutine per
// pipeline: they have separate databases, so they never contend, and a slow
// job in one pipeline must not stall another's queue.
func (r *LocalRunner) Drain(ctx context.Context, pipelines []*Pipeline) {
	var wg sync.WaitGroup

	for _, target := range pipelines {
		wg.Add(1)

		go func() {
			defer wg.Done()

			r.drainPipeline(ctx, target)
		}()
	}

	wg.Wait()
}

func (r *LocalRunner) drainPipeline(ctx context.Context, target *Pipeline) {
	slog.Info("web.pipeline.drain_start", "pipeline", target.Slug)

	// Anything a previous process left claimed-but-unfinished is stranded
	// until something releases it — the same recovery `steps watch` does at
	// startup, for the same reason.
	err := target.Store.ResetStaleRunning(ctx)
	if err != nil {
		slog.Error("web.reset_stale", "pipeline", target.Slug, "error", err)
	}

	err = target.Store.SyncSerialGroups(ctx, target.Cfg.SerialGroupsByJob())
	if err != nil {
		slog.Error("web.sync_serial_groups", "pipeline", target.Slug, "error", err)
	}

	for {
		if ctx.Err() != nil {
			return
		}

		ran := r.drainOne(ctx, target)
		if ran {
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(drainIdleBackoff):
		}
	}
}

// drainOne claims at most one queued job and runs it. It reports whether it
// ran anything, so the caller knows whether to back off.
func (r *LocalRunner) drainOne(ctx context.Context, target *Pipeline) bool {
	id, jobName, claimed, err := target.Store.ClaimNextJob(ctx)
	if err != nil {
		slog.Error("web.claim", "pipeline", target.Slug, "error", err)

		return false
	}

	if !claimed {
		return false
	}

	job, err := target.Cfg.FindJob(jobName)
	if err != nil {
		_ = target.Store.CompleteJob(ctx, id, "failed", err)

		return true
	}

	force := r.takeForce(target.Slug, jobName)

	slog.Info("web.job.run", "pipeline", target.Slug, "job", jobName)

	runErr := r.runJob(ctx, target, job, force)

	status := "succeeded"
	if runErr != nil {
		status = "failed"

		slog.Error("web.run", "pipeline", target.Slug, "job", jobName, "error", runErr)
	}

	slog.Info("web.job.done", "pipeline", target.Slug, "job", jobName, "status", status)

	err = target.Store.CompleteJob(context.WithoutCancel(ctx), id, status, runErr)
	if err != nil {
		slog.Error("web.complete", "pipeline", target.Slug, "job", jobName, "error", err)
	}

	return true
}

// runJob executes one job with this pipeline's bus attached, so the run's
// events reach any browser watching it.
func (r *LocalRunner) runJob(ctx context.Context, target *Pipeline, job *config.Job, force bool) error {
	provider, ok := r.providers[target.Slug]
	if !ok {
		// A pipeline with no provider was never registered, which means the
		// server was built by hand (a test) rather than by the command. Refuse
		// rather than inventing a workspace: running a job somewhere the
		// caller did not choose is worse than not running it.
		return fmt.Errorf("web: no workspace provider registered for pipeline %q", target.Slug)
	}

	//nolint:wrapcheck // the run error is reported to the queue row verbatim
	return pipeline.RunJob(events.WithBus(ctx, target.Bus), target.Cfg, job, nil, provider, target.Store, force)
}
