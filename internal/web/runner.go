package web

// Making the UI able to act, not only report.
//
// A trigger from the web goes into the same durable queue `steps web` uses,
// and is drained by the same pipeline.RunJob every other path calls. There is
// deliberately no second execution route: a job started from a browser must
// be the same job, with the same caching, hooks, serial groups, and recording,
// as one started from a terminal.

import (
	"context"
	"errors"
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
//
// A quarter second rather than the one it was: this is a SELECT against a
// local sqlite queue, so four a second per pipeline costs nothing measurable,
// and the number is the latency a person watching the follow page pays
// between pressing trigger and the run appearing. A second of that is visible;
// a quarter is not.
const drainIdleBackoff = 250 * time.Millisecond

// LocalRunner drains each pipeline's queue in this process. It is the Runner
// the `steps web` command installs; a read-only server has none.
type LocalRunner struct {
	providers *providers
	// pinned is --pin, the version fields every run this process starts is
	// held to. A property of the process, not of a request: the operator
	// pinned the daemon, and a job triggered from the browser is still that
	// daemon running it.
	pinned map[string]string
	// concurrent bounds how many of one pipeline's queued jobs run at once.
	// It was `steps web --max-concurrent`, and it is about keeping a daemon
	// responsive while a slow build runs — serial:/max_in_flight are the
	// pipeline's own limits and are enforced in SQL by ClaimNextJob, below
	// whatever this allows.
	concurrent int
	// force is --force: every job this process drains ignores the cache,
	// however it was enqueued. A property of the process like pinned, and
	// separate from `forced` below, which is one browser request asking for
	// one re-run.
	force bool
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
	// running is how an abort reaches a run: its cancel, held for exactly as long as RunJob is.
	running map[runKey]context.CancelCauseFunc
}

// runKey scopes a run id to its pipeline, so a URL naming one pipeline cannot stop another's run.
type runKey struct{ slug, runID string }

// errAborted is what separates somebody stopping a run from the daemon going down: both cancel the same context, and only one of them is an outcome.
var errAborted = errors.New("aborted on request")

// NewLocalRunner builds a runner over per-pipeline workspace providers, keyed
// by pipeline slug. concurrent below 1 means one job at a time.
func NewLocalRunner(
	initial map[string]workspace.Provider, pinned map[string]string, concurrent int, force bool,
) *LocalRunner {
	if concurrent < 1 {
		concurrent = 1
	}

	held := newProviders()
	for slug, provider := range initial {
		held.set(slug, provider)
	}

	return &LocalRunner{
		providers:  held,
		pinned:     pinned,
		concurrent: concurrent,
		force:      force,
		forced:     map[string]bool{},
		running:    map[runKey]context.CancelCauseFunc{},
	}
}

// SetProvider installs the workspace a pipeline's runs materialize in, retiring whatever it replaces once the runs holding it finish.
func (r *LocalRunner) SetProvider(slug string, provider workspace.Provider) {
	r.providers.set(slug, provider)
}

// RemoveProvider retires a destroyed pipeline's workspace, once nothing is running in it.
func (r *LocalRunner) RemoveProvider(slug string) { r.providers.remove(slug) }

// Close retires every workspace this runner holds. Only a provider's own Close removes the tree it created, so a shutdown that closed the stores alone left a steps-* directory per pipeline behind with nothing to reap it.
func (r *LocalRunner) Close() { r.providers.Close() }

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

// Abort only cancels: the run still unwinds through its on_abort and ensure hooks, and gives back its serial slot when it actually ends.
func (r *LocalRunner) Abort(target *Pipeline, runID string) bool {
	r.mu.Lock()
	cancel, running := r.running[runKey{target.Slug, runID}]
	r.mu.Unlock()

	if running {
		cancel(errAborted)
	}

	return running
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

			r.DrainPipeline(ctx, target)
		}()
	}

	wg.Wait()
}

// DrainPipeline runs one pipeline's queue with `concurrent` workers.
//
// Workers rather than one loop because a daemon that is mid-build stops
// noticing everything else: the queue this drains is also where a browser
// trigger and an approval release land, and one long agent step used to make
// all of them wait. What it does NOT do is loosen the pipeline's own limits —
// ClaimNextJob admits in a single statement against serial: and
// max_in_flight, so extra workers find nothing to claim rather than running
// what the pipeline forbade.
func (r *LocalRunner) DrainPipeline(ctx context.Context, target *Pipeline) {
	slog.Info("web.pipeline.drain_start", "pipeline", target.Slug, "workers", r.concurrent)

	var wg sync.WaitGroup

	for range r.concurrent {
		wg.Add(1)

		go func() {
			defer wg.Done()

			r.drainWorker(ctx, target)
		}()
	}

	wg.Wait()
}

// drainWorker is one worker's loop: claim and run until the queue is empty,
// then back off and try again.
func (r *LocalRunner) drainWorker(ctx context.Context, target *Pipeline) {
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

// PrepareQueue is the startup recovery `steps web` does, mirrored here so
// recovery happens the same way regardless of which front end drains the
// queue. It is recovery ONLY: what a configuration says about who may run is
// SyncQueueLimits' job, because that has to happen again on every reload while
// this must happen exactly once.
//
// The caller runs this BEFORE starting any drain or poll goroutine, which is
// a requirement rather than a convention: ResetStaleRunning is three
// statements with no transaction around them, and an enqueue landing between
// two of them leaves a row no later poll re-queues.
//
// It recovers unconditionally, which is a statement about deployment rather
// than a shortcut: recovery reads every `running` row as an abandoned
// leftover, and `steps web` is the only daemon there is — so a row it finds
// running at startup belongs to a process that is gone. Two of them against
// one state file would each undo the other, which is the deployment mistake
// the one-process-per-database rule names. It was once answered by a file
// lock this process raced for, and then by --no-watch, which went with the
// separate watcher; see store.ResetStaleRunning for why the lock went.
func PrepareQueue(ctx context.Context, target *Pipeline) {
	err := target.Store.ResetStaleRunning(ctx)
	if err != nil {
		slog.Error("web.reset_stale", "pipeline", target.Slug, "error", err)
	}
}

// SyncQueueLimits mirrors the served configuration's admission rules into the
// tables ClaimNextJob reads.
//
// NOT part of PrepareQueue, though it once was: it belongs to ADOPTING a
// configuration, which happens at startup and at every reload, while recovery
// belongs to starting the process. ResetStaleRunning reads every `running`
// row as an abandoned leftover, which is true once, at startup, and false
// while this process is mid-build. Everything a swap changes about who may
// run — a job joining a serial group, a max_in_flight raised or removed —
// lives in SQL rather than in the Config, so a configuration adopted without
// this serves pages promising a `serial:` the queue goes on ignoring.
func SyncQueueLimits(ctx context.Context, target *Pipeline) {
	cfg := target.Config()

	err := target.Store.SyncJobLimits(ctx, cfg.SerialGroupsByJob(), cfg.MaxInFlightByJob())
	if err != nil {
		slog.Error("web.sync_job_limits", "pipeline", target.Slug, "error", err)
	}
}

// drainOne claims at most one queued job and runs it. It reports whether it
// ran anything, so the caller knows whether to back off.
func (r *LocalRunner) drainOne(ctx context.Context, target *Pipeline) bool {
	if !admits(ctx, target) {
		return false
	}

	id, jobName, claimed, err := target.Store.ClaimNextJob(ctx)
	if err != nil {
		slog.Error("web.claim", "pipeline", target.Slug, "error", err)

		return false
	}

	if !claimed {
		return false
	}

	// One read of the served configuration for the whole claim, threaded from
	// here. Two reads is two configurations: the watcher swaps this pointer
	// under the drain, and a job resolved from one while the plan executes
	// against another is a combination that existed in no file on disk.
	cfg := target.Config()

	job, err := cfg.FindJob(jobName)
	if err != nil {
		// Detached, like every other finalize here: a job the config no
		// longer names is terminal, and a SIGINT at this instant must not
		// strand the row running with nothing recorded.
		_ = target.Store.CompleteJob(context.WithoutCancel(ctx), id, "failed", err)

		return true
	}

	if r.skipIfPaused(ctx, target, job.Name, id) {
		return true
	}

	r.runAndFinalize(ctx, target, cfg, job, id)

	return true
}

// runAndFinalize executes one claimed job and records how it went.
func (r *LocalRunner) runAndFinalize(
	ctx context.Context, target *Pipeline, cfg *config.Config, job *config.Job, id int64,
) {
	force := r.takeForce(target.Slug, job.Name) || r.force

	slog.Info("web.job.run", "pipeline", target.Slug, "job", job.Name)

	aborted, runErr := r.runJob(ctx, target, cfg, job, force)

	// Ahead of the interrupted case, which it also is: left running, the next startup re-runs a build somebody stopped. Not the job's own outcome either, so the breaker neither counts nor clears it.
	if runErr != nil && aborted {
		slog.Info("web.job.aborted", "pipeline", target.Slug, "job", job.Name)

		err := target.Store.CompleteJob(context.WithoutCancel(ctx), id, "aborted", runErr)
		if err != nil {
			slog.Error("web.complete", "pipeline", target.Slug, "job", job.Name, "error", err)
		}

		return
	}

	// A run the process's own shutdown cut short is not an outcome. Store's
	// CompleteJob says so in its doc comment, and the contract is load-bearing
	// in both directions: the row must stay `running` so the next startup's
	// ResetStaleRunning re-queues it (nothing else would — only a new version
	// change enqueues), and it must not count against the circuit breaker,
	// because an operator pressing ctrl-C is not the job being broken.
	if runErr != nil && interrupted(runErr) {
		slog.Warn("web.job.interrupted", "pipeline", target.Slug, "job", job.Name)

		return
	}

	status := "succeeded"
	if runErr != nil {
		status = "failed"

		slog.Error("web.run", "pipeline", target.Slug, "job", job.Name, "error", runErr)
	}

	slog.Info("web.job.done", "pipeline", target.Slug, "job", job.Name, "status", status)

	err := target.Store.CompleteJob(context.WithoutCancel(ctx), id, status, runErr)
	if err != nil {
		slog.Error("web.complete", "pipeline", target.Slug, "job", job.Name, "error", err)
	}

	r.recordBreaker(ctx, target, job, runErr)
}

// admits reports whether the pipeline-level breaker lets anything be claimed.
//
// Asked before the claim rather than after it: a paused pipeline admits
// nothing, so a row queued before the pause waits rather than running.
func admits(ctx context.Context, target *Pipeline) bool {
	stopped, err := target.Store.Paused(ctx)
	if err != nil {
		slog.Error("web.paused", "pipeline", target.Slug, "error", err)

		return false
	}

	return !stopped
}

// interrupted reports a run that stopped because the process is going down,
// rather than because the job answered.
func interrupted(runErr error) bool {
	return errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded)
}

// skipIfPaused finalizes a queued row for a job the circuit breaker has taken
// out of the rotation, rather than running it.
//
// It lives here as well as in internal/trigger because this is the drainer the
// daemon actually uses: `max_consecutive_failures:` is documented against
// `steps web`, and a breaker that only the one-shot honours is a safety
// feature that is off in the mode it exists for.
func (r *LocalRunner) skipIfPaused(ctx context.Context, target *Pipeline, jobName string, id int64) bool {
	paused, err := target.Store.IsJobPaused(ctx, jobName)
	if err != nil {
		slog.Warn("web.breaker_error", "pipeline", target.Slug, "job", jobName, "error", err)

		return false
	}

	if !paused {
		return false
	}

	slog.Warn("web.job.paused", "pipeline", target.Slug, "job", jobName,
		"resume", "steps jobs resume "+jobName+" -p <pipeline>")

	err = target.Store.CompleteJob(context.WithoutCancel(ctx), id, "skipped", nil)
	if err != nil {
		slog.Error("web.complete", "pipeline", target.Slug, "job", jobName, "error", err)
	}

	return true
}

// recordBreaker advances (or clears) a job's consecutive-failure count and
// says so loudly when the breaker trips.
//
// Loudly is the requirement: the whole point is that somebody should know the
// job stopped, and a daemon's scrollback is where that is least likely to be
// noticed — which is why `steps jobs` exists to be asked afterwards.
func (r *LocalRunner) recordBreaker(ctx context.Context, target *Pipeline, job *config.Job, runErr error) {
	// Detached: the outcome is already terminal, and a SIGINT arriving here
	// must not lose the count a later run reasons about.
	paused, consecutive, err := target.Store.RecordJobOutcome(
		context.WithoutCancel(ctx), job.Name, runErr == nil, job.MaxConsecutiveFailures)
	if err != nil {
		slog.Warn("web.breaker_error", "pipeline", target.Slug, "job", job.Name, "error", err)

		return
	}

	if runErr == nil || job.MaxConsecutiveFailures <= 0 || !paused {
		return
	}

	slog.Warn("web.job_paused",
		"pipeline", target.Slug,
		"job", job.Name,
		"consecutive_failures", consecutive,
		"max_consecutive_failures", job.MaxConsecutiveFailures,
		"resume", "steps jobs resume "+job.Name+" -p <pipeline>")
}

// runJob executes one job with this pipeline's bus attached, so the run's
// events reach any browser watching it.
func (r *LocalRunner) runJob(
	ctx context.Context, target *Pipeline, cfg *config.Config, job *config.Job, force bool,
) (bool, error) {
	provider, release, ok := r.providers.take(target.Slug)
	if !ok {
		// Refused rather than inventing a workspace: an unregistered pipeline is a server built by hand, and running somewhere nobody chose is worse than not running.
		return false, fmt.Errorf("web: no workspace provider registered for pipeline %q", target.Slug)
	}

	// Held for the life of the run, so a set that swaps the workspace under this pipeline cannot close the tree this build is materializing into.
	defer release()

	// Minted here rather than inside RunJob so the run is addressable before it exists: an abort can land during preflight.
	runID := pipeline.NewRunID()
	key := runKey{target.Slug, runID}

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	r.mu.Lock()
	r.running[key] = cancel
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.running, key)
		r.mu.Unlock()
	}()

	runErr := pipeline.RunJob(
		events.WithBus(pipeline.WithNewRun(runCtx, runID), target.Bus), cfg, job, r.pinned, provider, target.Store, force)

	return errors.Is(context.Cause(runCtx), errAborted), runErr
}
