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
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/actor"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/redact"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/store"
)

// Resources is every resource a poll checks; the predicate lives in config.Config.PolledResourceNames because internal/pipeline needs it and cannot import this package.
func Resources(cfg *config.Config) []string {
	return cfg.PolledResourceNames()
}

// AffectedJobs returns every job that has a trigger:true get step resolving to
// resourceName, in declaration order. A job with more than one such step on
// the same resource is returned once. Matching is on the resolved resource
// name (get: aliases included), matching Resources — including a trigger:true
// get nested inside an in_parallel:/race: branch, which config.Job.TriggersOn
// walks the same tree config.PolledResourceNames does to find.
func AffectedJobs(cfg *config.Config, resourceName string) []*config.Job {
	jobs := make([]*config.Job, 0)

	for i := range cfg.Jobs {
		job := &cfg.Jobs[i]

		if job.TriggersOn(resourceName) {
			jobs = append(jobs, job)
		}
	}

	return jobs
}

// PollStore is what polling touches: the queue it fills, the versions a check compares against, and whether the pipeline is paused.
type PollStore interface {
	store.Queue
	store.Versions
	store.Control
	store.Pruning
}

// Poll is the producer half of the daemon: it checks every trigger: true
// resource on an interval and enqueues the jobs a version change affects,
// until ctx is canceled. It never drains the queue — `steps web` drains it
// through its own in-process runner, and a second set of workers here would
// claim rows out from under it.
//
// Two things stay the caller's:
//
//   - startup reconciliation (ResetStaleRunning and the serial-group /
//     max-in-flight syncs). Whoever owns the drain owns that, since it is the
//     drain's admission it repairs.
//   - the configuration, as a SOURCE rather than a value. A `steps pipeline
//     set` replaces it while this loop runs, and a loop holding the config it
//     started with would go on checking resources the operator deleted and
//     never check the ones they added. Each cycle reads the current one, and
//     admission re-validates and re-preflights whenever it changes.
//
// requests, when not nil, is how a person asks for a check between ticks.
func Poll(ctx context.Context, current ConfigSource, st PollStore, interval time.Duration, requests *Requests) error {
	if interval <= 0 {
		return fmt.Errorf("trigger: interval must be positive, got %s", interval)
	}

	runPoller(ctx, current, st, interval, requests)

	return nil
}

// requestBacklog bounds the checks a pipeline holds asked for and not yet taken.
const requestBacklog = 16

// Requests carries a person's "check this now" to a pipeline's running poller, which takes it between ticks. Through the loop rather than beside it because the loop is the pipeline's one checker: a requested check run alongside a scheduled one would race it over the same resource's recorded version and history.
type Requests struct {
	names chan string
	set   chan struct{}
}

// NewRequests is a pipeline's empty backlog.
func NewRequests() *Requests {
	return &Requests{names: make(chan string, requestBacklog), set: make(chan struct{}, 1)}
}

// Set tells the poller its configuration was replaced, so it adopts the new one now rather than at its next tick: a run started in between would otherwise refresh a moved resource from the old source's cursor (steps#280).
func (r *Requests) Set() {
	select {
	case r.set <- struct{}{}:
	default:
	}
}

// Check asks for resource to be checked and reports false when the backlog is full — a request is a nudge, and those already waiting will do.
func (r *Requests) Check(resource string) bool {
	select {
	case r.names <- resource:
		return true
	default:
		return false
	}
}

// ConfigSource hands out the configuration to poll against, which the daemon
// swaps under this loop when the pipeline is set again.
type ConfigSource func() *config.Config

// preflightTriggers proves every trigger resource can actually be checked,
// and every model and MCP server the pipeline's agents need can actually be
// reached, before anything is polled or written.
//
// A trigger resource whose check can never succeed — an mcp: tool the server
// does not expose, or one whose required arguments this pipeline never sends
// — is a permanent misconfiguration, and the poll loop's own reaction to it
// (log the error, wait, try again) hides that for as long as the daemon
// runs: nothing red, nothing running, nothing enqueued, forever. Say it once,
// up front, and leave the configuration unpolled until it changes.
//
// The agents are checked here for the same reason and against a quieter
// failure: their servers were only ever probed by the per-job preflight, so
// an unusable one surfaced at the first trigger — inside a job, after its
// gets had already run — rather than at the startup that could have refused
// to begin. See pipeline.PreflightPipeline.
//
// Only what is knowable without running anything, so a shell-backed check —
// whose correctness is whatever the command does — reports nothing here and
// still fails per-poll the way it always has.
func preflightTriggers(ctx context.Context, cfg *config.Config, resources []string) error {
	problems := pipeline.PreflightPipeline(ctx, cfg, resources)
	if len(problems) == 0 {
		return nil
	}

	// Only a problem that WAITING cannot fix stops this configuration being
	// polled. A server that did not answer, or a token a refresh would have
	// renewed, is said once and then left to the poll loop, which retries by
	// its very nature — refusing there is how a poller that should have come
	// back on its own is found idle on Monday. A tool the server does not
	// expose still refuses: no interval will grow it one.
	var (
		out      strings.Builder
		terminal bool
	)

	out.WriteString("trigger preflight failed, nothing was polled:")

	for _, problem := range problems {
		if problem.Transient {
			slog.WarnContext(ctx, "trigger.preflight_transient", "target", problem.Target, "detail", problem.Detail)

			continue
		}

		terminal = true

		fmt.Fprintf(&out, "\n  %s: %s", problem.Target, problem.Detail)
	}

	if !terminal {
		return nil
	}

	return errors.New(out.String())
}

// runPoller calls pollOnce immediately and then once per interval tick,
// until ctx is canceled.
func runPoller(ctx context.Context, current ConfigSource, st PollStore, interval time.Duration, requests *Requests) {
	admitted := &admission{}
	adopted := &adoption{}

	if cfg := current(); adopted.adopt(ctx, cfg, st) {
		admitted.poll(ctx, cfg, st)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// A nil channel is never ready, so a loop nobody can ask is the plain ticker.
	var (
		asked <-chan string
		set   <-chan struct{}
	)

	if requests != nil {
		asked, set = requests.names, requests.set
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Read per cycle, not captured: see Poll.
			cfg := current()
			if adopted.adopt(ctx, cfg, st) {
				admitted.poll(ctx, cfg, st)
			}
		case name := <-asked:
			cfg := current()
			if adopted.adopt(ctx, cfg, st) {
				checkRequested(ctx, cfg, st, name)
			}
		case <-set:
			adopted.adopt(ctx, current(), st)
		}
	}
}

// adoption retires what each configuration stopped declaring and files its resource types and sources, once, from the poller's own goroutine: between polls, so no check still running under the configuration being replaced can write the old source's versions or cursor after the reset (steps#280).
type adoption struct {
	// filed is the configuration last adopted, compared by identity as admission's is.
	filed *config.Config
}

// adopt reports whether cfg's history is ready to poll; a failure is retried at the next tick rather than polled through, which would hand a moved resource the old source's cursor.
func (a *adoption) adopt(ctx context.Context, cfg *config.Config, st PollStore) bool {
	if cfg == a.filed {
		return true
	}

	err := pipeline.Adopt(ctx, st, cfg)
	if err != nil {
		slog.ErrorContext(ctx, "trigger.adopt", "pipeline", cfg.Name, "error", err)

		return false
	}

	a.filed = cfg

	return true
}

// checkRequested is one resource's turn in a poll, taken because a person asked. Any resource, not only a trigger: true one — Concourse's check button checks whatever it is pressed on — and without admission's preflight, whose job is to stop a loop repeating a failure every tick: a person asked once, and a failure is recorded for them to read. A paused pipeline checks nothing, asked or not.
func checkRequested(ctx context.Context, cfg *config.Config, st PollStore, name string) {
	ctx = events.WithLogAttrs(ctx, "revision", cfg.Revision.SHA)

	stopped, err := st.Paused(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "trigger.paused", "pipeline", cfg.Name, "error", err)

		return
	}

	if stopped {
		return
	}

	_, err = cfg.FindResource(name)
	if err != nil {
		// Removed by a set between the request and now.
		slog.WarnContext(ctx, "trigger.check_requested", "pipeline", cfg.Name, "resource", name, "error", err)

		return
	}

	pollAndLog(ctx, cfg, st, []string{name})
}

// admission decides, once per configuration rather than once per cycle,
// whether this loop has anything it can poll.
//
// Once per CONFIGURATION is the whole design. The check it gates —
// preflightTriggers — proves a trigger resource can be checked at all, and it
// used to run once before the loop started, which was right when a
// configuration lasted as long as the process. Under a daemon whose pipelines
// are set again while it runs, it was wrong in both directions: a resource added by an edit was never
// preflighted, so an unsatisfiable one logged the same error every cycle
// forever — exactly the failure the check exists to prevent — and a pipeline
// that gained its FIRST trigger: true get was never polled at all, because
// the decision not to poll had been taken at startup and never revisited.
//
// A configuration that cannot be polled is not an error the loop exits on. It
// is a state the loop sits in until the pipeline is set again, which is the
// only thing that can change the answer.
type admission struct {
	// decided is the configuration this verdict is about, compared by
	// identity: a set stores a new pointer, and nothing else does.
	decided  *config.Config
	pollable bool
}

func (a *admission) poll(ctx context.Context, cfg *config.Config, st PollStore) {
	// Per cycle, on this call's own ctx: the loop's would grow by one layer a tick for the life of the daemon.
	ctx = events.WithLogAttrs(ctx, "revision", cfg.Revision.SHA)

	// The pipeline-level breaker, asked per cycle rather than per configuration: a pause is not an edit, and the whole point of it is that it takes effect without one.
	stopped, err := st.Paused(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "trigger.paused", "pipeline", cfg.Name, "error", err)

		return
	}

	if stopped {
		return
	}

	if cfg != a.decided {
		a.decided = cfg
		a.pollable = a.decide(ctx, cfg)
	}

	if !a.pollable {
		return
	}

	pollAndLog(ctx, cfg, st, Resources(cfg))
}

// decide reports whether this configuration can be polled, and says out loud
// what it decided — once per configuration, which is what makes it readable
// rather than a line a second.
//
// Every line names the pipeline, because the daemon serves several and this
// verdict changes while it runs: "polling 3 resources" beside "nothing to
// poll" is worse than no banner when neither says which pipeline gave up.
func (a *admission) decide(ctx context.Context, cfg *config.Config) bool {
	name := cfg.Name

	resources := Resources(cfg)
	if len(resources) == 0 {
		// Not a failure: plenty of pipelines are run by hand, and an edit may
		// add a trigger later — which is why the loop stays.
		slog.InfoContext(ctx, "trigger.nothing_to_poll", "reason", "no get step sets trigger: true")

		return false
	}

	err := pipeline.ValidatePipelinePlacement(ctx, cfg, resources)
	if err != nil {
		slog.ErrorContext(ctx, "trigger.unpollable", "pipeline", name, "error", err)

		return false
	}

	err = preflightTriggers(ctx, cfg, resources)
	if err != nil {
		slog.ErrorContext(ctx, "trigger.unpollable", "pipeline", name, "error", err)

		return false
	}

	slog.InfoContext(ctx, "trigger.polling", "resources", len(resources))

	return true
}

func pollAndLog(ctx context.Context, cfg *config.Config, st PollStore, names []string) {
	ctx, release := pipeline.CheckScope(ctx)
	defer release()

	enqueued, err := pollResources(ctx, cfg, st, names)

	for _, name := range enqueued {
		slog.InfoContext(ctx, "trigger.enqueued", "job", name)
	}

	for _, failure := range pollFailures(err) {
		// The failure surfaces here, outside the resource's scope, so it carries its resource back.
		failureCtx := ctx

		var named checkFailure
		if errors.As(failure, &named) {
			failureCtx = events.WithLogAttrs(ctx, "resource", named.resource)
		}

		slog.ErrorContext(failureCtx, "trigger.poll", "error", failure)
	}
}

// pollFailures splits a poll's error into one per failing resource, so each line names its own.
func pollFailures(err error) []error {
	if err == nil {
		return nil
	}

	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}

	return []error{err}
}

// observedResource is one trigger resource's latest version as seen by a
// single poll, plus whether that version is a change from what was last
// recorded (dirty) — carried so pollOnce can enqueue affected jobs *before*
// advancing the recorded version (see pollOnce).
type observedResource struct {
	// version is the decoded latest version, kept alongside its JSON so a
	// passed: constraint can be checked without re-parsing.
	version map[string]any
	latest  store.VersionKey
	dirty   bool
	// versions is everything this check returned, oldest first.
	versions []map[string]any
	// coldStart marks a resource nothing had recorded before this check —
	// the one moment a backlog is not news. See recordHistory.
	coldStart bool
	// delivered marks a webhook resource, whose checked version a delivery may move mid-poll; previous is what it read, for the compare-and-set that advances it.
	delivered     bool
	previous      store.VersionKey
	previousFound bool
}

// pollOnce is pollResources over every trigger resource.
func pollOnce(ctx context.Context, cfg *config.Config, st PollStore) ([]string, error) {
	return pollResources(ctx, cfg, st, Resources(cfg))
}

// pollResources checks each named resource once and enqueues (deduplicated)
// every job affected by a resource whose latest version changed since the
// last recorded check. It returns the job names it enqueued, in sorted
// order.
//
// Each resource is checked independently, as Concourse does: one whose check
// fails is left out of this poll's observations — filed by
// recordCheckOutcome and never advanced, so what it missed is still dirty
// when it recovers — and the rest trigger as usual. The failures come back
// joined, beside whatever was enqueued.
//
// Ordering is load-bearing for correctness: the recorded version is advanced
// only *after* every affected job has been durably enqueued, so if anything
// fails partway (EnqueueJob failing) every resource stays "dirty" and the
// trigger is retried on the next poll (at-least-once) rather than silently
// consumed. A resource checked for the
// first time ever seeds a baseline and is never itself considered dirty on
// that first check — this keeps a fresh (or freshly lost) state db from
// mass-triggering every job on daemon startup.
func pollResources(ctx context.Context, cfg *config.Config, st PollStore, names []string) ([]string, error) {
	observed := map[string]observedResource{}

	var failures []error

	for _, name := range names {
		// Shutdown fails every check left; stopping keeps one cancellation from becoming a failure per resource.
		if ctx.Err() != nil {
			return nil, errors.Join(append(failures, ctx.Err())...)
		}

		obs, hasVersion, err := observeRecovering(events.WithLogAttrs(ctx, "resource", name), cfg, st, name)
		if err != nil {
			failures = append(failures, checkFailure{resource: name, err: err})

			continue
		}

		if hasVersion {
			observed[name] = obs
		}
	}

	enqueued, err := enqueueAffected(ctx, cfg, st, observed)
	if err != nil {
		return nil, errors.Join(append(failures, err)...)
	}

	// Then release anything a passed: constraint was holding back. This runs
	// on EVERY poll, against the current version rather than a changed one:
	// the poll that first sees a version is necessarily earlier than the
	// upstream job going green on it, so a constrained job is held back and
	// the recorded version then advances past it. Waiting for the resource to
	// go dirty again would mean waiting for a version nobody has tested yet —
	// which is to say, forever.
	released, err := releaseConstrainedJobs(ctx, cfg, st, enqueued)
	if err != nil {
		return nil, errors.Join(append(failures, err)...)
	}

	enqueued = append(enqueued, released...)

	// Only now — after every affected job is durably queued — advance each
	// resource's recorded version. A failure here returns the jobs already
	// enqueued and leaves the not-yet-recorded resources dirty for retry.
	for resourceName, obs := range observed {
		err := advance(ctx, st, resourceName, obs)
		if err != nil {
			return enqueued, errors.Join(append(failures, checkFailure{resource: resourceName, err: err})...)
		}
	}

	sort.Strings(enqueued)

	return enqueued, errors.Join(failures...)
}

// checkFailure is a resource's turn in a poll failing — its check or its advance — carrying which resource so the line reporting it can say so. Its text is the underlying error's, which already names the resource.
type checkFailure struct {
	resource string
	err      error
}

func (f checkFailure) Error() string { return f.err.Error() }

func (f checkFailure) Unwrap() error { return f.err }

// observeRecovering is observe with a panic turned into that resource's check failure: the poller's goroutine has no recover above it (resource.CheckShare re-panics a check's), so a built-in type's panic took the whole daemon down.
func observeRecovering(ctx context.Context, cfg *config.Config, st PollStore, name string) (obs observedResource, hasVersion bool, err error) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}

		slog.ErrorContext(ctx, "trigger.check_panic", "resource", name, "panic", recovered, "stack", string(debug.Stack()))

		obs, hasVersion = observedResource{}, false
		err = fmt.Errorf("trigger resource %q: check panicked: %v", name, recovered)

		recordCheckOutcome(ctx, st, name, err)
	}()

	return observe(ctx, cfg, st, name)
}

// observe is one resource's turn in a poll: a check whose findings are filed into history, or — for a webhook resource, which has no check — what the deliveries already say.
func observe(ctx context.Context, cfg *config.Config, st PollStore, name string) (observedResource, bool, error) {
	if isWebhook(cfg, name) {
		return observeDeliveries(ctx, st, name)
	}

	obs, hasVersion, err := checkResource(ctx, cfg, st, name)

	recordCheckOutcome(ctx, st, name, err)

	if err != nil || !hasVersion {
		return obs, hasVersion, err
	}

	grew, err := recordHistory(ctx, cfg, st, name, obs)
	if err != nil {
		return observedResource{}, false, err
	}

	// New versions BELOW the head are still new work: the head comparison
	// alone would file them into history and trigger nothing.
	obs.dirty = obs.dirty || grew

	return obs, true, nil
}

// recordCheckOutcome files why a check failed, or clears what a previous one
// filed.
//
// It exists because a failing check leaves its resource's recorded version
// exactly where it was — the poll skips it and carries on with the others —
// and otherwise the only trace is a log line on whichever machine runs the
// daemon. A reader looking at the UI saw a resource whose last-checked time
// was merely getting older, which is indistinguishable from a quiet one.
//
// Its own failure is logged rather than returned: this is a diagnostic about
// a poll, and losing the real error to report a bookkeeping one would trade a
// reason somebody can act on for one nobody can.
//
// It writes under the CALLER's context, which is the opposite of what a
// cleanup path does here and is deliberate. Shutting the daemon down cancels
// every check in flight; a WithoutCancel would file what those return, and
// the next start would report "context canceled" against half the pipeline's
// resources — an alarm about the operator's own Ctrl-C, raised on a surface
// whose entire value is being quiet when nothing is wrong. Cancelled, the
// write simply fails and is logged, which is the right amount of fuss.
func recordCheckOutcome(ctx context.Context, st PollStore, resourceName string, checkErr error) {
	var message string
	if checkErr != nil {
		// Stored and shown on the resource's page: a check that quoted its own credential in a failure must not keep it.
		message = redact.SecretsFrom(ctx).String(checkErr.Error())
	}

	err := st.RecordCheckError(ctx, resourceName, message)
	if err != nil {
		slog.ErrorContext(ctx, "trigger.check_error_record", "resource", resourceName, "error", err)
	}
}

// advance records a resource's observed version as checked. A delivery can move a webhook resource's checked version mid-poll, having dispatched itself; writing back what observe read would rewind past it and build it again next poll. So a clean one is left alone, and a dirty one moves only from what was read — a lost compare-and-set is that delivery already in place.
func advance(ctx context.Context, st PollStore, resourceName string, obs observedResource) error {
	var err error

	switch {
	case !obs.delivered:
		err = st.RecordCheckedVersion(ctx, resourceName, obs.latest)
	case obs.dirty:
		_, err = st.CompareAndSetCheckedVersion(ctx, store.CheckedSwap{Resource: resourceName, Expected: obs.previous, ExpectedFound: obs.previousFound, Next: obs.latest})
	}

	if err != nil {
		return fmt.Errorf("record version for %q: %w", resourceName, err)
	}

	return nil
}

// enqueueAffected enqueues, once each, every job affected by a dirty resource
// in observed, returning the job names enqueued. It runs before any version
// is recorded, so a failure here leaves every resource dirty for retry.
func enqueueAffected(ctx context.Context, cfg *config.Config, st PollStore, observed map[string]observedResource) ([]string, error) {
	reasons, err := affectedJobs(ctx, cfg, st, observed)
	if err != nil {
		return nil, err
	}

	enqueued := make([]string, 0, len(reasons))

	for jobName, reason := range reasons {
		err := st.EnqueueJob(ctx, jobName, reason, actor.Check(reason).String())
		if err != nil {
			return nil, fmt.Errorf("enqueue job %q: %w", jobName, err)
		}

		enqueued = append(enqueued, jobName)
	}

	return enqueued, nil
}

// affectedJobs decides which jobs a poll's findings should run, and why.
//
// The reason is a label and the first dirty resource will do; which versions
// a job then builds is not decided here at all, but read from history when
// the job runs (see internal/pipeline's loadResourceHistory).
func affectedJobs(
	ctx context.Context, cfg *config.Config, st PollStore, observed map[string]observedResource,
) (map[string]string, error) {
	reasons := map[string]string{}

	for resourceName, obs := range observed {
		if !obs.dirty {
			continue
		}

		for _, job := range AffectedJobs(cfg, resourceName) {
			if _, already := reasons[job.Name]; already {
				continue
			}

			due, err := jobDue(ctx, st, job)
			if err != nil {
				return nil, err
			}

			if !due {
				continue
			}

			reasons[job.Name] = resourceName
		}
	}

	return reasons, nil
}

// releaseConstrainedJobs enqueues every passed:-constrained job that has a
// trigger: true get, once the versions it would build have gone green
// upstream and it has not itself run its trigger inputs.
//
// Only a trigger: true get starts a job, as in Concourse, where a build is
// created only when a trigger input's version is a first occurrence
// (atc/scheduler/scheduler.go, ensurePendingBuildExists). A job whose gets are
// all passed: without trigger: is started by a person, never by its upstream
// going green.
//
// The guard against releasing the same work forever is the job's own builds of its trigger inputs, passed or failed (see jobAlreadyRanThese): a failing job waits for a new trigger version or a person, as in Concourse, rather than running every poll until a breaker holds it.
func releaseConstrainedJobs(
	ctx context.Context, cfg *config.Config, st PollStore, alreadyEnqueued []string,
) ([]string, error) {
	var released []string

	for i := range cfg.Jobs {
		job := &cfg.Jobs[i]

		if len(job.PassedConstraints()) == 0 || slices.Contains(alreadyEnqueued, job.Name) {
			continue
		}

		due, err := jobDue(ctx, st, job)
		if err != nil {
			return nil, err
		}

		if !due {
			continue
		}

		err = st.EnqueueJob(ctx, job.Name, "upstream jobs passed this version", actor.Upstream().String())
		if err != nil {
			return nil, fmt.Errorf("enqueue job %q: %w", job.Name, err)
		}

		released = append(released, job.Name)
	}

	return released, nil
}

// jobDue reports whether a poll should start a job: its passed: gates hold, and a gated job has no queued or claimed build still to take its versions and some trigger input no build of it has used. A check's new version asks it as the release does, or a version upstream has not yet passed rebuilds the gated set the job already used.
func jobDue(ctx context.Context, st PollStore, job *config.Job) (bool, error) {
	set, ready, err := passedSetFor(ctx, st, job)
	if err != nil || !ready || len(job.PassedConstraints()) == 0 {
		return ready, err
	}

	// Holding is safe because a gated job is judged again every poll by releaseConstrainedJobs.
	pending, err := st.QueuedWithoutInputs(ctx, job.Name)
	if err != nil || pending {
		return false, err //nolint:wrapcheck // the store names the job
	}

	done, err := jobAlreadyRanThese(ctx, st, job, set)

	return !done, err
}

// jobAlreadyRanThese reports whether a build of the job, however it ended, already took each version its trigger: true gets would build (gated ones from the set that passed together, the rest at their newest), each on its own: Concourse's IsFirstOccurrence asks per input over build_resource_config_version_inputs and filters on no status. A trigger input with no version holds the job, having nothing to build with; a job with no trigger: true get has vacuously run, since only a person starts it.
func jobAlreadyRanThese(ctx context.Context, st PollStore, job *config.Job, set map[string]store.VersionKey) (bool, error) {
	for _, input := range job.Inputs() {
		if !input.Trigger {
			continue
		}

		version, found := set[input.Resource]
		if !found {
			var err error

			version, found, err = st.NewestVersion(ctx, input.Resource)
			if err != nil {
				return false, fmt.Errorf("trigger input %q of job %q: %w", input.Resource, job.Name, err)
			}
		}

		if !found {
			return true, nil
		}

		runs, err := st.VersionRuns(ctx, input.Resource, []store.VersionKey{version})
		if err != nil {
			return false, fmt.Errorf("trigger input %q of job %q: %w", input.Resource, job.Name, err)
		}

		if !slices.ContainsFunc(runs, func(run store.VersionRun) bool { return run.Run.JobName == job.Name }) {
			return false, nil
		}
	}

	return true, nil
}

// jobReadyFor reports whether some set of versions satisfies every passed:
// constraint the job declares at once.
//
// This is the correctness gap passed: exists to close: without it, the poll loop can
// trigger `deploy` on a commit the `test` job ALREADY FAILED on, and there is
// no way to say "don't deploy unless the tests were green for this exact
// commit".
//
// A job held back is not an error and not a lost trigger: the green record
// is durable, so the poll after the upstream job goes green enqueues it —
// even if newer, unproven versions have arrived on top in the meantime.
func jobReadyFor(ctx context.Context, st PollStore, job *config.Job) (bool, error) {
	_, ready, err := passedSetFor(ctx, st, job)

	return ready, err
}

// passedSetFor is the set a released job would build its gated gets from:
// per upstream job one green build holding every resource it vouches for,
// which is the version set internal/pipeline's loadResourceHistory resolves
// the build to, so what is judged here and what is built there agree.
//
// Reading the store rather than this poll's observation is the point. The
// observed head is one version, and judging only it meant a head that kept
// failing upstream starved the job forever while a validated version sat in
// history. Taking each resource's newest green version independently was the
// other half of the same mistake: a fan-in whose newest versions never passed
// TOGETHER was held forever while an older set that did sat in history.
func passedSetFor(ctx context.Context, st PollStore, job *config.Job) (map[string]store.VersionKey, bool, error) {
	constraints := job.PassedConstraints()

	set, found, err := st.PassedSet(ctx, constraints)
	if err != nil {
		return nil, false, fmt.Errorf("passed: constraint for job %q: %w", job.Name, err)
	}

	if !found {
		slog.InfoContext(ctx, "trigger.waiting_on_passed", "job", job.Name, "resources", slices.Sorted(maps.Keys(constraints)))
	}

	return set, found, nil
}

// checkResource runs resourceName's check command and reports its latest
// version and whether that version differs from the previously recorded one.
// It deliberately does *not* record the version — pollOnce advances the
// recorded version only after affected jobs are enqueued, so a failure
// between check and enqueue can't silently consume a change. hasVersion is
// false when the check returned no versions at all (nothing to record or
// trigger on).
//
// The previously recorded version does double duty. It is the cursor handed
// to the check itself, so a type can ask its API for what it has not seen
// rather than guessing a window; and, compared against what the check
// returned, it is the dirty bit that decides whether to enqueue. Reading it
// once serves both.
func checkResource(ctx context.Context, cfg *config.Config, st PollStore, resourceName string) (obs observedResource, hasVersion bool, err error) {
	resource, err := cfg.FindResource(resourceName)
	if err != nil {
		return observedResource{}, false, fmt.Errorf("trigger resource %q: %w", resourceName, err)
	}

	resourceType, err := cfg.FindResourceType(resource.Type)
	if err != nil {
		return observedResource{}, false, fmt.Errorf("trigger resource %q: %w", resourceName, err)
	}

	previous, cursor, found, err := recordedVersion(ctx, st, resourceName)
	if err != nil {
		return observedResource{}, false, err
	}

	// Opened before placement, so a worker a check had to start is in the check's trace.
	ctx, endCheck := events.StartCheck(ctx, resourceName)
	defer func() { endCheck(err) }()

	ctx, err = pipeline.PlaceResource(ctx, cfg, resourceName)
	if err != nil {
		return observedResource{}, false, fmt.Errorf("trigger resource %q: %w", resourceName, err)
	}

	versions, err := rsrc.CheckVersions(ctx, rsrc.Call{Config: cfg, Type: *resourceType, Resource: *resource, Version: cursor, Recorded: filedVersions(st, resourceName)})
	if err != nil {
		return observedResource{}, false, fmt.Errorf("trigger resource %q: %w", resourceName, err)
	}

	// A check that reports nothing is not a resource without a version — it
	// is a resource whose version has not changed, and the recorded one is
	// still current. Saying otherwise drops the resource out of `observed`,
	// and a passed: constraint on it can then never be evaluated: it needs
	// the CURRENT version, not a changed one (see releaseConstrainedJobs).
	//
	// This is the steady state for any cursor-driven check, which asks only
	// for what it has not seen and therefore answers with nothing almost
	// every poll. Before the cursor a check re-reported its whole window
	// every time and this could not arise, which is why it took a
	// cursor-driven pipeline to expose it. Concourse has the same notion for
	// free: its version DB always knows a resource's current version,
	// whatever the latest check happened to return.
	if len(versions) == 0 {
		if !found {
			return observedResource{}, false, nil
		}

		return observedResource{version: cursor, latest: previous, dirty: false}, true, nil
	}

	latest, err := store.EncodeVersion(versions[len(versions)-1])
	if err != nil {
		return observedResource{}, false, fmt.Errorf("trigger resource %q: %w", resourceName, err)
	}

	return observedResource{
		version:   versions[len(versions)-1],
		latest:    latest,
		dirty:     found && previous != latest,
		versions:  versions,
		coldStart: !found,
	}, true, nil
}

// filedVersions reads a resource's version history for a check that reports what it filed before as it filed it (rsrc.Call.Recorded).
func filedVersions(st PollStore, resourceName string) func(context.Context) ([]map[string]any, error) {
	return func(ctx context.Context) ([]map[string]any, error) {
		filed, err := st.ResourceVersionsJSON(ctx, resourceName)
		if err != nil {
			return nil, err //nolint:wrapcheck // the store names the resource
		}

		return store.DecodeVersions(filed)
	}
}

// recordedVersion reads the version last recorded for a resource, decoded.
// It is the check cursor and, when a check reports nothing new, still the
// resource's current version.
func recordedVersion(
	ctx context.Context, st PollStore, resourceName string,
) (encoded store.VersionKey, version map[string]any, found bool, err error) {
	last, found, err := st.LastChecked(ctx, resourceName)
	if err != nil {
		return "", nil, false, fmt.Errorf("trigger resource %q: %w", resourceName, err)
	}

	if !found {
		return "", nil, false, nil
	}

	encoded = last.Version

	version, err = store.DecodeVersion(encoded)
	if err != nil {
		return "", nil, false, fmt.Errorf("trigger resource %q: %w", resourceName, err)
	}

	return encoded, version, true, nil
}

// recordHistory files everything this check reported, reports whether any of
// it was NEW to history, and on a resource's first-ever check seeds the
// baseline so a backlog is not answered.
//
// The newness answer is what the trigger's dirty bit cannot see on its own:
// that bit compares only the HEAD version against the baseline, so a check
// whose window backfills older versions — a re-listed tag, an
// eventually-consistent API — would file them into history and then trigger
// nothing, leaving an every-mode backlog idle until some future head change.
// New-to-history is the real "something arrived" signal, so it feeds dirty
// alongside the head comparison (see pollOnce).
//
// Cold-start seeding marks the whole first report as already taken, for every
// job that reads the resource: history makes every version buildable, which
// is the point, but a job whose plan says version: every would then fan out
// over the entire backlog the first time anything triggered it. steps has
// always drawn the line there — a cold start records without enqueuing — and
// this draws it for the per-job cursor too.
//
// The BASELINE is recorded here as well, immediately, not left to pollOnce's
// end-of-poll loop. That loop deliberately baselines only after affected jobs
// are enqueued, so a crash cannot skip work — but a cold start enqueues
// nothing, so there is nothing that ordering protects, and waiting is
// actively harmful: if a LATER resource's check fails, the poll aborts before
// the baseline lands, the resource reads as cold again next poll, and the
// re-seed marks versions that arrived in between as taken — never enqueued,
// silently dropped, forever. Seeding and baselining together closes that to
// a crash-width window.
//
// The known edge, stated rather than discovered: a job ADDED to the pipeline
// later has no seeding, so its first trigger fans out over whatever history
// holds. steps cannot tell a new job from one that has simply not run; the
// cure is a narrower version_history: or a first run under --pin.
//
// A get switched from latest to version: every is NOT that edge: seeding
// marks every job that reads the resource, whatever version mode its get
// used, so the cursor already holds the cold-start mark and the first every
// run fans only over what arrived since. The backlog is bounded by the seed,
// not by version_history:.
func recordHistory(
	ctx context.Context, cfg *config.Config, st PollStore, resourceName string, obs observedResource,
) (bool, error) {
	if len(obs.versions) == 0 {
		return false, nil
	}

	added, err := st.RecordVersions(ctx, resourceName, obs.versions, cfg.VersionHistoryLimit())
	if err != nil {
		return false, fmt.Errorf("trigger resource %q: %w", resourceName, err)
	}

	if !obs.coldStart {
		return added > 0, nil
	}

	err = seedColdStart(ctx, cfg, st, resourceName, obs.latest)
	if err != nil {
		return false, err
	}

	// Said out loud because the check's own "versions=N" line reads as N
	// pending triggers to anyone who doesn't know the cold-start rule — and
	// a first poll after deleting the state db is exactly when someone is
	// staring at the log deciding whether to ^C.
	slog.InfoContext(ctx, "trigger.cold_start", "resource", resourceName, "versions_recorded", len(obs.versions),
		"note", "first-ever check: everything below the newest recorded as already taken; the newest triggers once")

	// The newest version IS news, which is what Concourse does with the
	// single version a first check reports (its check contract returns only
	// the current version when given none, so a Concourse first check has no
	// backlog to consider). Only the backlog BELOW it is swallowed — see
	// seedColdStart.
	return true, nil
}

// seedColdStart marks a first-seen resource's whole history as taken for
// every job that reads it, and records the baseline in the same breath — see
// recordHistory for why neither may wait for the end of the poll.
func seedColdStart(ctx context.Context, cfg *config.Config, st PollStore, resourceName string, latest store.VersionKey) error {
	orders, err := st.VersionOrders(ctx, resourceName)
	if err != nil {
		return fmt.Errorf("trigger resource %q: %w", resourceName, err)
	}

	mark := coldStartMark(orders, latest)

	for i := range cfg.Jobs {
		job := &cfg.Jobs[i]
		if !job.GetsResource(resourceName) {
			continue
		}

		err = st.RecordConsumedMark(ctx, job.Name, resourceName, mark)
		if err != nil {
			return fmt.Errorf("trigger resource %q: %w", resourceName, err)
		}
	}

	err = st.RecordCheckedVersion(ctx, resourceName, latest)
	if err != nil {
		return fmt.Errorf("trigger resource %q: %w", resourceName, err)
	}

	return nil
}

// coldStartMark is how much of a first-ever check counts as already taken:
// everything BELOW the newest version, so the newest is left for the poll to
// trigger exactly once.
//
// Marking the whole window (which this did until the rule changed) meant a
// fresh — or freshly deleted — state database could never build anything
// until something new arrived, which for a resource that changes slowly is
// indistinguishable from a poll loop that does not work. Concourse builds the
// version its first check reports; this is the same outcome for a check that
// reports a window rather than only its current version.
//
// The mark is a high-water mark over DISCOVERY order, so this is the largest
// order strictly below the newest's. Orders are not assumed contiguous
// (RecordVersionOrder assigns MAX+1, and a pin can mint one), hence a scan
// rather than "highest - 1". A latest that is somehow not the highest order
// falls back to seeding everything: refusing to build is the safe direction
// when the ordering says something this function does not understand.
func coldStartMark(orders map[store.VersionKey]int64, latest store.VersionKey) int64 {
	var highest, second int64

	for _, order := range orders {
		switch {
		case order > highest:
			highest, second = order, highest
		case order > second && order < highest:
			second = order
		}
	}

	latestOrder, ok := orders[latest]
	if !ok || latestOrder != highest {
		return highest
	}

	return second
}
