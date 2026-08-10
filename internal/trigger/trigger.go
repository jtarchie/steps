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
	"maps"
	"runtime/debug"
	"slices"
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
// A get step's resource: alias is resolved to the underlying resource name
// (see config.Step.GetResourceName), so two gets aliasing the same resource
// poll it once and a version change affects every job that references it.
func Resources(cfg *config.Config) []string {
	seen := map[string]bool{}

	names := make([]string, 0)

	for _, job := range cfg.Jobs {
		for _, step := range job.Plan {
			// trigger: true is the reason to poll. passed: is the other one:
			// a constrained get needs its version OBSERVED for the constraint
			// to be judged at all, even when a change to it triggers nothing.
			// Without this a `get: artifacts, passed: [build]` with no
			// trigger: was never checked, so jobReadyFor saw no version and
			// held the job back for the lifetime of the watcher, silently.
			//
			// Polling it enqueues nothing on its own: AffectedJobs still
			// requires trigger: true, so a change here cannot start a build
			// that never asked to be started by it.
			if step.Get == "" || (!step.Trigger && len(step.Passed) == 0) {
				continue
			}

			name := step.GetResourceName()
			if seen[name] {
				continue
			}

			seen[name] = true

			names = append(names, name)
		}
	}

	return names
}

// AffectedJobs returns every job that has a trigger:true get step resolving to
// resourceName, in declaration order. A job with more than one such step on
// the same resource is returned once. Matching is on the resolved resource
// name (get: aliases included), matching Resources.
func AffectedJobs(cfg *config.Config, resourceName string) []*config.Job {
	jobs := make([]*config.Job, 0)

	for i := range cfg.Jobs {
		job := &cfg.Jobs[i]

		for _, step := range job.Plan {
			if step.GetResourceName() == resourceName && step.Trigger {
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
	listenAddr string,
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

	// Serial-group membership lives in the database so the claim can stay one
	// atomic statement (see Store.ClaimNextJob). Sync it from the pipeline as
	// it is right now, so a group removed from the YAML stops holding a lock.
	err = st.SyncSerialGroups(ctx, cfg.SerialGroupsByJob())
	if err != nil {
		return fmt.Errorf("watch: %w", err)
	}

	var wg sync.WaitGroup

	// The webhook listener runs alongside the poll loop, never instead of it:
	// a missed delivery must not mean a change is never noticed.
	if listenAddr != "" {
		wg.Add(1)

		go func() {
			defer wg.Done()

			serveErr := serveWebhooks(ctx, cfg, st, listenAddr)
			if serveErr != nil {
				slog.Error("webhook.stopped", "error", serveErr)
			}
		}()
	}

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
	// version is the decoded latest version, kept alongside its JSON so a
	// passed: constraint can be checked without re-parsing.
	version map[string]any
	latest  string
	dirty   bool
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

	// Then release anything a passed: constraint was holding back. This runs
	// on EVERY poll, against the current version rather than a changed one:
	// the poll that first sees a version is necessarily earlier than the
	// upstream job going green on it, so a constrained job is held back and
	// the recorded version then advances past it. Waiting for the resource to
	// go dirty again would mean waiting for a version nobody has tested yet —
	// which is to say, forever.
	released, err := releaseConstrainedJobs(ctx, cfg, st, observed, enqueued)
	if err != nil {
		return nil, err
	}

	enqueued = append(enqueued, released...)

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
			if _, already := reasons[job.Name]; already {
				continue
			}

			ready, err := jobReadyFor(ctx, st, job, observed)
			if err != nil {
				return nil, err
			}

			if !ready {
				continue
			}

			reasons[job.Name] = resourceName
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

// reportSerialWaits says which pending jobs are held by a serial-group lock,
// and who holds it.
//
// Without this a blocked job is indistinguishable from an idle watcher:
// nothing is running for that job, nothing is being said, and the operator
// cannot tell "nobody has picked this up" from "something is holding the
// lock". Best-effort — a reporting failure must not fail the drain.
func reportSerialWaits(ctx context.Context, cfg *config.Config, st *store.Store) {
	for i := range cfg.Jobs {
		job := &cfg.Jobs[i]

		if !job.Serial && len(job.SerialGroups) == 0 {
			continue
		}

		holder, err := st.SerialGroupHolder(ctx, job.Name)
		if err != nil || holder == "" || holder == job.Name {
			continue
		}

		fmt.Printf("trigger: %s waiting: lock held by %s\n", job.Name, holder)
		slog.Info("trigger.serial_wait", "job", job.Name, "held_by", holder)
	}
}

// releaseConstrainedJobs enqueues every passed:-constrained job whose upstream
// jobs have now gone green on the version currently observed.
//
// The guard against enqueueing the same work forever is the job's OWN passed
// record: once it succeeds against a version it has recorded one, so it stops
// being releasable for that version. A job that keeps failing is re-attempted
// one at a time (the queue holds at most one pending row per job) until the
// circuit breaker pauses it, which is what the breaker is for.
func releaseConstrainedJobs(
	ctx context.Context, cfg *config.Config, st *store.Store,
	observed map[string]observedResource, alreadyEnqueued []string,
) ([]string, error) {
	var released []string

	for i := range cfg.Jobs {
		job := &cfg.Jobs[i]

		constraints := job.PassedConstraints()
		if len(constraints) == 0 || slices.Contains(alreadyEnqueued, job.Name) {
			continue
		}

		ready, err := jobReadyFor(ctx, st, job, observed)
		if err != nil {
			return nil, err
		}

		if !ready {
			continue
		}

		done, err := jobAlreadyRanThese(ctx, st, job.Name, constraints, observed)
		if err != nil {
			return nil, err
		}

		if done {
			continue
		}

		err = st.EnqueueJob(ctx, job.Name, "upstream jobs passed this version")
		if err != nil {
			return nil, fmt.Errorf("enqueue job %q: %w", job.Name, err)
		}

		released = append(released, job.Name)
	}

	return released, nil
}

// jobAlreadyRanThese reports whether the job has itself already succeeded
// against every constrained resource's current version — the fact that stops a
// released job being released again on the next poll.
func jobAlreadyRanThese(
	ctx context.Context, st *store.Store, jobName string,
	constraints map[string][]string, observed map[string]observedResource,
) (bool, error) {
	want, complete := observedSetFor(constraints, observed)
	if !complete {
		return false, nil
	}

	passed, err := pipeline.VersionSetPassedUpstream(ctx, st, jobName, want)
	if err != nil {
		return false, fmt.Errorf("passed: constraint for job %q: %w", jobName, err)
	}

	return passed, nil
}

// jobReadyFor reports whether every passed: constraint the job declares is
// satisfied by the versions currently observed.
//
// This is the correctness gap passed: exists to close: without it, watch can
// trigger `deploy` on a commit the `test` job ALREADY FAILED on, and there is
// no way to say "don't deploy unless the tests were green for this exact
// commit".
//
// A job held back is not an error and not a lost trigger: the version stays
// current, so the next poll after the upstream job goes green enqueues it.
func jobReadyFor(ctx context.Context, st *store.Store, job *config.Job, observed map[string]observedResource) (bool, error) {
	// Inverted from resource -> upstream jobs into upstream job -> the set of
	// constrained resources naming it. That inversion IS the fix: each
	// upstream job is asked once, about every version it vouches for at once,
	// so a combination that passed only in pieces is refused. Asking per
	// resource could never see the combination at all.
	for _, upstreamJob := range upstreamJobsOf(job) {
		constrained := map[string][]string{}

		for resource, upstream := range job.PassedConstraints() {
			if slices.Contains(upstream, upstreamJob) {
				constrained[resource] = upstream
			}
		}

		want, complete := observedSetFor(constrained, observed)
		if !complete {
			// A constrained resource was not checked this poll (it carries no
			// trigger: true), so there is no version to judge. Holding the job
			// back is the conservative reading, and the one that matches "only
			// run against a version that passed".
			return false, nil
		}

		passed, err := pipeline.VersionSetPassedUpstream(ctx, st, upstreamJob, want)
		if err != nil {
			return false, fmt.Errorf("passed: constraint for job %q: %w", job.Name, err)
		}

		if !passed {
			names := slices.Sorted(maps.Keys(want))

			fmt.Printf("trigger: %s waiting — %s has not gone green against this combination of %v yet\n", job.Name, upstreamJob, names)
			slog.Info("trigger.waiting_on_passed", "job", job.Name, "upstream", upstreamJob, "resources", names)

			return false, nil
		}
	}

	return true, nil
}

// upstreamJobsOf lists every job named by any of this job's passed:
// constraints, in a stable order.
func upstreamJobsOf(job *config.Job) []string {
	seen := map[string]bool{}

	for _, upstream := range job.PassedConstraints() {
		for _, name := range upstream {
			seen[name] = true
		}
	}

	return slices.Sorted(maps.Keys(seen))
}

// observedSetFor collects the currently observed version of every constrained
// resource. complete is false when any of them was not checked this poll —
// there is then no set to ask about, and the caller holds the job back.
func observedSetFor(constraints map[string][]string, observed map[string]observedResource) (want map[string]map[string]any, complete bool) {
	want = make(map[string]map[string]any, len(constraints))

	for resource := range constraints {
		obs, seen := observed[resource]
		if !seen {
			return nil, false
		}

		want[resource] = obs.version
	}

	return want, true
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

	versions, err := rsrc.CheckVersions(ctx, cfg, *resourceType, resource.Source)
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

	return observedResource{
		version: versions[len(versions)-1],
		latest:  string(latest),
		dirty:   found && previous != string(latest),
	}, true, nil
}

// recoverDrainPanic turns a value recovered from a panic in drainOne into the
// error it should report, finalizing the claimed job (if any) as "failed" via
// CompleteJob first — the same outcome as any other failure — so a panic
// doesn't leave a claimed row stuck running forever with nothing to finalize
// it. claimed is false when the panic happened before ClaimNextJob returned a
// row (or ClaimNextJob itself panicked), in which case there's no row to
// finalize.
func recoverDrainPanic(ctx context.Context, st *store.Store, jobName string, id int64, claimed bool, r any) error {
	slog.Error("trigger.panic", "job", jobName, "recovered", r, "stack", string(debug.Stack()))

	panicErr := fmt.Errorf("recovered from panic running job %q: %v", jobName, r)

	if !claimed {
		return panicErr
	}

	completeErr := st.CompleteJob(context.WithoutCancel(ctx), id, "failed", panicErr)
	if completeErr != nil {
		return fmt.Errorf("%w (and could not record failure: %w)", panicErr, completeErr)
	}

	return panicErr
}

// finalizeMissingJob records a terminal failure for a queued job whose name
// no longer resolves in cfg (removed from the pipeline between enqueue and
// claim), returning the error drainOne should report.
func finalizeMissingJob(ctx context.Context, st *store.Store, jobName string, id int64, findErr error) error {
	completeErr := st.CompleteJob(context.WithoutCancel(ctx), id, "failed", findErr)
	if completeErr != nil {
		return fmt.Errorf("triggered job %q: %w (and could not record failure: %w)", jobName, findErr, completeErr)
	}

	return fmt.Errorf("triggered job %q: %w", jobName, findErr)
}

// wasInterruptedByCancellation reports whether runErr stems from ctx being
// canceled during the command that produced it (internal/shell's
// wrapIfCanceled/CanceledError ensure such an error's chain wraps ctx.Err()),
// as opposed to "is ctx canceled right now" — the latter would also be true
// for a genuine failure that merely happens to coincide with an unrelated
// cancellation (an operator restarting the daemon at the same moment a task
// fails on its own), which must still be recorded failed, not silently
// dropped as if it were the cancellation's doing.
func wasInterruptedByCancellation(runErr error) bool {
	return errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded)
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
) (ran bool, err error) {
	var (
		id      int64
		jobName string
		found   bool
		claimed bool
	)

	// A panic anywhere below — including three layers deep inside
	// pipeline.RunJob's resource/task/agent code — must not crash the whole
	// watch process and silently take every other in-flight worker down with
	// it. Recover around the entire claim-run-complete sequence, not just the
	// RunJob call, so a panic in ClaimNextJob/CompleteJob itself is covered
	// too, and so a panic after a successful claim still gets a best-effort
	// CompleteJob("failed", ...) — the same outcome as any other failure —
	// instead of leaving the row claimed but never finalized.
	defer func() {
		r := recover()
		if r == nil {
			return
		}

		ran = true
		err = recoverDrainPanic(ctx, st, jobName, id, claimed, r)
	}()

	id, jobName, found, err = st.ClaimNextJob(ctx)
	if err != nil {
		return false, fmt.Errorf("claim next job: %w", err)
	}

	if !found {
		// Nothing claimable. That is usually an empty queue, but it is also
		// what a serial-group lock looks like — and "queued" and "blocked on
		// a lock" are different states a reader has to be able to tell apart.
		reportSerialWaits(ctx, cfg, st)

		return false, nil
	}

	claimed = true

	job, err := cfg.FindJob(jobName)
	if err != nil {
		// A queued job that no longer resolves (removed from config between
		// enqueue and claim) is a genuine, terminal failure — finalize it with
		// a detached context so a racing cancellation can't strand the row.
		return true, finalizeMissingJob(ctx, st, jobName, id, err)
	}

	skipped, err := skipIfPaused(ctx, st, jobName, id)
	if skipped || err != nil {
		return true, err
	}

	fmt.Printf("trigger: running %s\n", jobName)

	runCtx, release := buildContext(ctx, job)
	defer release()

	return true, finalizeRun(ctx, st, job, id, pipeline.RunJob(runCtx, cfg, job, pinned, provider, st, force))
}

// nonInterruptibleGrace bounds how long a shutdown waits for a build that did
// not opt into interruption.
//
// A bound rather than an open-ended wait, because the alternative is a watcher
// that cannot be stopped: a job with no timeout: of its own and a hung command
// would hold shutdown forever, and "kill -9 the supervisor" is not a shutdown
// story. Ten minutes is longer than any deploy this is meant to protect and
// short enough that an operator waiting on it does not assume the process is
// wedged. A job that needs longer should say so with its own timeout:, which
// still applies inside RunJob.
const nonInterruptibleGrace = 10 * time.Minute

// buildContext gives a triggered build the cancellation behaviour its job
// asked for, mirroring Concourse's interruptible: — see config.Job.
//
// interruptible: true (or an already-dead ctx) keeps today's behaviour: the
// build shares the watcher's context and dies with it.
//
// The default detaches from cancellation and takes a deadline instead, so a
// SIGTERM during a deploy lets that deploy finish rather than leaving the
// outside world half-changed. Watch's own WaitGroup then holds shutdown until
// the worker returns, which is what makes the wait real rather than advisory.
func buildContext(ctx context.Context, job *config.Job) (context.Context, context.CancelFunc) {
	if job.Interruptible {
		return ctx, func() {}
	}

	return context.WithTimeout(context.WithoutCancel(ctx), nonInterruptibleGrace)
}

// finalizeRun records a completed triggered run: its queue row, its breaker
// count, and the error the caller reports.
func finalizeRun(ctx context.Context, st *store.Store, job *config.Job, id int64, runErr error) error {
	// A job interrupted by ctx-cancellation (SIGINT/SIGTERM mid-run) isn't a
	// real failure: leave its row running so the next watch startup's
	// ResetStaleRunning re-queues it, rather than marking it failed and
	// silently dropping it (only a new version change would otherwise ever
	// re-trigger it). This is specifically the *interrupted* case — see
	// wasInterruptedByCancellation. A job that reached a terminal state
	// (below) is finalized even if cancellation is racing it. It is also not
	// counted against the breaker: an operator pressing ctrl-C is not the job
	// being broken.
	if runErr != nil && wasInterruptedByCancellation(runErr) {
		return fmt.Errorf("triggered job %q: %w", job.Name, runErr)
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
		return fmt.Errorf("complete job %q: %w", job.Name, completeErr)
	}

	recordBreaker(ctx, st, job, runErr)

	if runErr != nil {
		return fmt.Errorf("triggered job %q: %w", job.Name, runErr)
	}

	return nil
}

// skipIfPaused finalizes a queued row for a job the breaker has taken out of
// the rotation, rather than leaving it pending — the queue would otherwise
// fill with work nobody intends to do.
func skipIfPaused(ctx context.Context, st *store.Store, jobName string, id int64) (bool, error) {
	paused, err := st.IsJobPaused(ctx, jobName)
	if err != nil {
		return false, fmt.Errorf("triggered job %q: %w", jobName, err)
	}

	if !paused {
		return false, nil
	}

	fmt.Printf("trigger: %s is paused (resume with: steps jobs resume %s)\n", jobName, jobName)

	err = st.CompleteJob(context.WithoutCancel(ctx), id, "skipped", nil)
	if err != nil {
		return true, fmt.Errorf("triggered job %q: %w", jobName, err)
	}

	return true, nil
}

// recordBreaker advances (or clears) a job's consecutive-failure count and
// says so loudly when the breaker trips.
//
// Loudly is the requirement, not a nicety: a breaker that trips silently
// defeats its own purpose, since the entire point is that someone should know
// this stopped. A broken nightly job left alone over a weekend fires four
// times and nobody finds out until a bill arrives.
//
// Best-effort by design: failing to record a breaker count must not turn a
// successful job into a failed one, or mask the real failure of a failed one.
func recordBreaker(ctx context.Context, st *store.Store, job *config.Job, runErr error) {
	// Detached: the outcome is already terminal, and a SIGINT arriving here
	// must not lose the count that a later run reasons about.
	recCtx := context.WithoutCancel(ctx)

	paused, consecutive, err := st.RecordJobOutcome(recCtx, job.Name, runErr == nil, job.MaxConsecutiveFailures)
	if err != nil {
		slog.Warn("trigger.breaker_error", "job", job.Name, "error", err)

		return
	}

	if runErr == nil || job.MaxConsecutiveFailures <= 0 {
		return
	}

	if !paused {
		fmt.Printf("trigger: %s failed (%d/%d consecutive)\n", job.Name, consecutive, job.MaxConsecutiveFailures)

		return
	}

	fmt.Printf("trigger: %s PAUSED after %d consecutive failures — resume with: steps jobs resume %s\n",
		job.Name, consecutive, job.Name)

	slog.Warn("trigger.job_paused",
		"job", job.Name,
		"consecutive_failures", consecutive,
		"max_consecutive_failures", job.MaxConsecutiveFailures,
		"resume", "steps jobs resume "+job.Name)
}
