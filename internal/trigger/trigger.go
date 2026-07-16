// Package trigger polls resources named by a get step's trigger: true and
// runs every job affected by a version change — the cross-job counterpart to
// internal/pipeline's single-job orchestration. A job is enqueued (into a
// durable, sqlite-backed queue on internal/store's Store) rather than run
// directly, so the poller and the job runner can operate as two independent
// loops: this is what gives durability (a crash mid-run doesn't lose track
// of what was pending) and a real concurrency cap, versus an in-memory
// dedup set that forgets everything on restart and has no limit.
package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/pipeline"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// workerIdleBackoff is how long an idle worker waits before checking the
// queue again, so an empty queue doesn't busy-spin between poll ticks.
const workerIdleBackoff = 500 * time.Millisecond

// Resources returns the distinct resource names referenced by any get
// step with trigger: true, anywhere in any job's plan, in first-seen order.
func Resources(cfg *config.Config) []string {
	seen := map[string]bool{}

	names := make([]string, 0)

	for _, job := range cfg.Jobs {
		for _, step := range job.Plan {
			if step.Get == "" || !step.Trigger || seen[step.Get] {
				continue
			}

			seen[step.Get] = true

			names = append(names, step.Get)
		}
	}

	return names
}

// AffectedJobs returns every job that has a trigger:true get step on
// resourceName, in declaration order. A job with more than one such step on
// the same resource is returned once.
func AffectedJobs(cfg *config.Config, resourceName string) []*config.Job {
	jobs := make([]*config.Job, 0)

	for i := range cfg.Jobs {
		job := &cfg.Jobs[i]

		for _, step := range job.Plan {
			if step.Get == resourceName && step.Trigger {
				jobs = append(jobs, job)

				break
			}
		}
	}

	return jobs
}

// Watch polls every trigger resource every interval, diffs against the last
// checked version recorded in st, and enqueues every job affected by a
// version change into st's durable queue. A pool of maxConcurrent workers
// (at least 1) drains that queue by calling pipeline.RunJob. Blocks until
// ctx is canceled.
func Watch(
	ctx context.Context,
	cfg *config.Config,
	provider workspace.Provider,
	st *store.Store,
	pinned map[string]string,
	interval time.Duration,
	maxConcurrent int,
	force bool,
) error {
	if len(Resources(cfg)) == 0 {
		return errors.New("no get step in any job sets trigger: true; nothing for watch to poll")
	}

	if interval <= 0 {
		return fmt.Errorf("watch: interval must be positive, got %s", interval)
	}

	if maxConcurrent < 1 {
		maxConcurrent = 1
	}

	// Recover any row a prior crash (or an interrupted graceful shutdown
	// mid-run — see drainOne) left stuck "running", so it isn't stranded
	// forever: only a new version change would otherwise ever re-queue it.
	err := st.ResetStaleRunning(ctx)
	if err != nil {
		return fmt.Errorf("watch: %w", err)
	}

	var wg sync.WaitGroup

	for range maxConcurrent {
		wg.Add(1)

		go func() {
			defer wg.Done()

			runWorker(ctx, cfg, provider, st, pinned, force)
		}()
	}

	runPoller(ctx, cfg, st, interval)

	wg.Wait()

	return nil
}

// runPoller calls pollOnce immediately and then once per interval tick,
// until ctx is canceled.
func runPoller(ctx context.Context, cfg *config.Config, st *store.Store, interval time.Duration) {
	pollAndLog(ctx, cfg, st)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pollAndLog(ctx, cfg, st)
		}
	}
}

func pollAndLog(ctx context.Context, cfg *config.Config, st *store.Store) {
	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil {
		slog.Error("trigger.poll", "error", err)

		return
	}

	for _, name := range enqueued {
		fmt.Printf("trigger: enqueued %s\n", name)
	}
}

// runWorker repeatedly drains the queue until ctx is canceled, backing off
// briefly whenever the queue is empty.
func runWorker(ctx context.Context, cfg *config.Config, provider workspace.Provider, st *store.Store, pinned map[string]string, force bool) {
	for {
		if ctx.Err() != nil {
			return
		}

		ran, err := drainOne(ctx, cfg, provider, st, pinned, force)
		if err != nil {
			slog.Error("trigger.run", "error", err)
		}

		if ran {
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(workerIdleBackoff):
		}
	}
}

// observedResource is one trigger resource's latest version as seen by a
// single poll, plus whether that version is a change from what was last
// recorded (dirty) — carried so pollOnce can enqueue affected jobs *before*
// advancing the recorded version (see pollOnce).
type observedResource struct {
	latest string
	dirty  bool
}

// pollOnce checks every trigger resource once and enqueues (deduplicated)
// every job affected by a resource whose latest version changed since the
// last recorded check. It returns the job names it enqueued, in sorted
// order.
//
// Ordering is load-bearing for correctness: the recorded version is advanced
// only *after* every affected job has been durably enqueued, so if anything
// fails partway (a later resource's check erroring, or EnqueueJob failing)
// the resource stays "dirty" and the trigger is retried on the next poll
// (at-least-once) rather than silently consumed. A resource checked for the
// first time ever seeds a baseline and is never itself considered dirty on
// that first check — this keeps a fresh (or freshly lost) state db from
// mass-triggering every job on watch startup.
func pollOnce(ctx context.Context, cfg *config.Config, st *store.Store) ([]string, error) {
	observed := map[string]observedResource{}

	for _, name := range Resources(cfg) {
		obs, hasVersion, err := checkResource(ctx, cfg, st, name)
		if err != nil {
			return nil, err
		}

		if hasVersion {
			observed[name] = obs
		}
	}

	enqueued, err := enqueueAffected(ctx, cfg, st, observed)
	if err != nil {
		return nil, err
	}

	// Only now — after every affected job is durably queued — advance each
	// resource's recorded version. A failure here returns the jobs already
	// enqueued and leaves the not-yet-recorded resources dirty for retry.
	for resourceName, obs := range observed {
		err := st.RecordCheckedVersion(ctx, resourceName, obs.latest)
		if err != nil {
			return enqueued, fmt.Errorf("record version for %q: %w", resourceName, err)
		}
	}

	sort.Strings(enqueued)

	return enqueued, nil
}

// enqueueAffected enqueues, once each, every job affected by a dirty resource
// in observed, returning the job names enqueued. It runs before any version
// is recorded, so a failure here leaves every resource dirty for retry.
func enqueueAffected(ctx context.Context, cfg *config.Config, st *store.Store, observed map[string]observedResource) ([]string, error) {
	reasons := map[string]string{}

	for resourceName, obs := range observed {
		if !obs.dirty {
			continue
		}

		for _, job := range AffectedJobs(cfg, resourceName) {
			if _, already := reasons[job.Name]; !already {
				reasons[job.Name] = resourceName
			}
		}
	}

	enqueued := make([]string, 0, len(reasons))

	for jobName, reason := range reasons {
		err := st.EnqueueJob(ctx, jobName, reason)
		if err != nil {
			return nil, fmt.Errorf("enqueue job %q: %w", jobName, err)
		}

		enqueued = append(enqueued, jobName)
	}

	return enqueued, nil
}

// checkResource runs resourceName's check command and reports its latest
// version and whether that version differs from the previously recorded one.
// It deliberately does *not* record the version — pollOnce advances the
// recorded version only after affected jobs are enqueued, so a failure
// between check and enqueue can't silently consume a change. hasVersion is
// false when the check returned no versions at all (nothing to record or
// trigger on).
func checkResource(ctx context.Context, cfg *config.Config, st *store.Store, resourceName string) (obs observedResource, hasVersion bool, err error) {
	resource, err := cfg.FindResource(resourceName)
	if err != nil {
		return observedResource{}, false, fmt.Errorf("trigger resource %q: %w", resourceName, err)
	}

	resourceType, err := cfg.FindResourceType(resource.Type)
	if err != nil {
		return observedResource{}, false, fmt.Errorf("trigger resource %q: %w", resourceName, err)
	}

	versions, err := rsrc.CheckVersions(ctx, *resourceType, resource.Source)
	if err != nil {
		return observedResource{}, false, fmt.Errorf("trigger resource %q: %w", resourceName, err)
	}

	if len(versions) == 0 {
		return observedResource{}, false, nil
	}

	latest, err := json.Marshal(versions[len(versions)-1])
	if err != nil {
		return observedResource{}, false, fmt.Errorf("trigger resource %q: could not marshal version: %w", resourceName, err)
	}

	previous, found, err := st.LastCheckedVersion(ctx, resourceName)
	if err != nil {
		return observedResource{}, false, fmt.Errorf("trigger resource %q: %w", resourceName, err)
	}

	return observedResource{latest: string(latest), dirty: found && previous != string(latest)}, true, nil
}

// drainOne claims one queued job (if any) and runs it via pipeline.RunJob.
// ran is false only when the queue was empty. A non-nil err is always
// worth logging but never a reason for the caller to stop draining — a
// failing job doesn't block the queue or other workers, it's simply not
// retried until the next real version change re-enqueues it.
func drainOne(
	ctx context.Context,
	cfg *config.Config,
	provider workspace.Provider,
	st *store.Store,
	pinned map[string]string,
	force bool,
) (bool, error) {
	id, jobName, found, err := st.ClaimNextJob(ctx)
	if err != nil {
		return false, fmt.Errorf("claim next job: %w", err)
	}

	if !found {
		return false, nil
	}

	job, err := cfg.FindJob(jobName)
	if err != nil {
		// A queued job that no longer resolves (removed from config between
		// enqueue and claim) is a genuine, terminal failure — finalize it with
		// a detached context so a racing cancellation can't strand the row.
		completeErr := st.CompleteJob(context.WithoutCancel(ctx), id, "failed", err)
		if completeErr != nil {
			return true, fmt.Errorf("triggered job %q: %w (and could not record failure: %w)", jobName, err, completeErr)
		}

		return true, fmt.Errorf("triggered job %q: %w", jobName, err)
	}

	fmt.Printf("trigger: running %s\n", jobName)

	runErr := pipeline.RunJob(ctx, cfg, job, pinned, provider, st, force)

	// A job interrupted by ctx-cancellation (SIGINT/SIGTERM mid-run) isn't a
	// real failure: leave its row running so the next watch startup's
	// ResetStaleRunning re-queues it, rather than marking it failed and
	// silently dropping it (only a new version change would otherwise ever
	// re-trigger it). This is specifically the *interrupted* case — a nonzero
	// return with ctx already canceled; a job that reached a terminal state
	// (below) is finalized even if cancellation is racing it.
	if runErr != nil && ctx.Err() != nil {
		return true, fmt.Errorf("triggered job %q: %w", jobName, runErr)
	}

	status := "done"
	if runErr != nil {
		status = "failed"
	}

	// Finalize with a context detached from cancellation: the job has reached
	// a terminal state (done, or a genuine failure with ctx still live), so
	// recording that outcome must not itself be aborted by a SIGINT arriving
	// at this instant — which would otherwise strand the row 'running' and
	// cause a spurious re-run on the next startup.
	completeErr := st.CompleteJob(context.WithoutCancel(ctx), id, status, runErr)
	if completeErr != nil {
		return true, fmt.Errorf("complete job %q: %w", jobName, completeErr)
	}

	if runErr != nil {
		return true, fmt.Errorf("triggered job %q: %w", jobName, runErr)
	}

	return true, nil
}
