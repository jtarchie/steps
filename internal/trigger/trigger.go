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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/pipeline"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/store"
)

// Resources returns the distinct resource names referenced by any get
// step with trigger: true, anywhere in any job's plan, in first-seen order.
// A get step's resource: alias is resolved to the underlying resource name
// (see config.Step.GetResourceName), so two gets aliasing the same resource
// poll it once and a version change affects every job that references it.
//
// Delegates to config.Config.PolledResourceNames, which carries the full
// doc on why passed: counts alongside trigger: true — internal/pipeline
// needs the same predicate (to know which resources NOT to touch when
// recording a run's resolved version) and cannot import this package, since
// this package already imports internal/pipeline.
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
}

// Poll is the producer half of a watching process: it validates and preflights
// the pipeline, then checks every trigger: true resource on an interval and
// enqueues the jobs a version change affects, until ctx is canceled. It never
// drains the queue.
//
// The split exists because `steps web` drains the same queue through its own
// in-process runner: a second set of workers here would claim rows out from
// under it, and the two would report each other's runs. It was once paired
// with a Watch that did both halves in this package; there is one daemon now,
// so only the producer half remains here.
//
// Three things stay the caller's, because the answer differs by front end:
//
//   - startup reconciliation (ResetStaleRunning and the serial-group /
//     max-in-flight syncs). Whoever owns the drain owns
//     that, since it is the drain's admission it repairs.
//   - the store handle. Pass the one the drain already uses: a Store is a
//     single pooled connection (SetMaxOpenConns(1)), so sharing it serializes
//     this loop's writes behind that pool instead of putting a second
//     connection on the same file to contend for its write lock. One handle
//     per pipeline, never one per loop.
//   - the configuration, as a SOURCE rather than a value. The daemon reloads
//     its pipeline file while this loop runs, and a loop holding the config
//     it started with would go on checking resources the operator deleted and
//     never check the ones they added — a poller quietly polling a file that
//     no longer exists. Each cycle takes the current one; only the startup
//     checks below read it once, because that is when they run.
func Poll(ctx context.Context, current ConfigSource, st PollStore, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("watch: interval must be positive, got %s", interval)
	}

	runPoller(ctx, current, st, interval)

	return nil
}

// ConfigSource hands out the configuration to poll against, which the daemon
// swaps under this loop when its pipeline file changes.
type ConfigSource func() *config.Config

// preflightTriggers proves every trigger resource can actually be checked,
// and every model and MCP server the pipeline's agents need can actually be
// reached, before anything is polled or written.
//
// A trigger resource whose check can never succeed — an mcp: tool the server
// does not expose, or one whose required arguments this pipeline never sends
// — is a permanent misconfiguration, and the poll loop's own reaction to it
// (log the error, wait, try again) hides that for as long as the watcher
// runs: nothing red, nothing running, nothing enqueued, forever. Say it once,
// at the top, and exit non-zero.
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

	// A watcher is a daemon, so only a problem that WAITING cannot fix earns
	// an exit. A server that did not answer, or a token a refresh would have
	// renewed, is said once and then left to the poll loop, which retries by
	// its very nature — quitting there is how a watcher that should have come
	// back on its own is found dead on Monday. A tool the server does not
	// expose still exits: no interval will grow it one.
	var (
		out      strings.Builder
		terminal bool
	)

	out.WriteString("trigger preflight failed, nothing was polled:")

	for _, problem := range problems {
		if problem.Transient {
			printf("trigger preflight: %s: %s (transient — polling anyway)\n", problem.Target, problem.Detail)
			slog.Warn("watch.preflight_transient", "target", problem.Target, "detail", problem.Detail)

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
func runPoller(ctx context.Context, current ConfigSource, st PollStore, interval time.Duration) {
	admitted := &admission{}

	admitted.poll(ctx, current(), st)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Read per cycle, not captured: see Poll.
			admitted.poll(ctx, current(), st)
		}
	}
}

// admission decides, once per configuration rather than once per cycle,
// whether this loop has anything it can poll.
//
// Once per CONFIGURATION is the whole design. The check it gates —
// preflightTriggers — proves a trigger resource can be checked at all, and it
// used to run once before the loop started, which was right when a
// configuration lasted as long as the process. Under a daemon that reloads it
// was wrong in both directions: a resource added by an edit was never
// preflighted, so an unsatisfiable one logged the same error every cycle
// forever — exactly the failure the check exists to prevent — and a pipeline
// that gained its FIRST trigger: true get was never polled at all, because
// the decision not to poll had been taken at startup and never revisited.
//
// A configuration that cannot be polled is not an error the loop exits on. It
// is a state the loop sits in until the file changes, which is the only thing
// that can change the answer.
type admission struct {
	// decided is the configuration this verdict is about, compared by
	// identity: a reload stores a new pointer, and nothing else does.
	decided  *config.Config
	pollable bool
}

func (a *admission) poll(ctx context.Context, cfg *config.Config, st PollStore) {
	// The pipeline-level breaker, asked per cycle rather than per configuration: a pause is not an edit, and the whole point of it is that it takes effect without one.
	stopped, err := st.Paused(ctx)
	if err != nil {
		slog.Error("trigger.paused", "pipeline", cfg.Name, "error", err)

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

	pollAndLog(ctx, cfg, st)
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
		printf("trigger: %s: nothing to poll — no get step sets trigger: true\n", name)

		return false
	}

	err := pipeline.ValidatePipelinePlacement(ctx, cfg, resources)
	if err != nil {
		slog.Error("trigger.unpollable", "pipeline", name, "error", err)

		return false
	}

	err = preflightTriggers(ctx, cfg, resources)
	if err != nil {
		slog.Error("trigger.unpollable", "pipeline", name, "error", err)

		return false
	}

	printf("trigger: %s: polling %d resource(s)\n", name, len(resources))

	return true
}

func pollAndLog(ctx context.Context, cfg *config.Config, st PollStore) {
	ctx, release := leasedChecks(ctx)
	defer release()

	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil {
		slog.Error("trigger.poll", "error", err)

		return
	}

	for _, name := range enqueued {
		printf("trigger: enqueued %s\n", name)
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
	// versions is everything this check returned, oldest first.
	versions []map[string]any
	// coldStart marks a resource nothing had recorded before this check —
	// the one moment a backlog is not news. See recordHistory.
	coldStart bool
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
func pollOnce(ctx context.Context, cfg *config.Config, st PollStore) ([]string, error) {
	observed := map[string]observedResource{}

	for _, name := range Resources(cfg) {
		obs, hasVersion, err := checkResource(ctx, cfg, st, name)
		if err != nil {
			return nil, err
		}

		if !hasVersion {
			continue
		}

		grew, err := recordHistory(ctx, cfg, st, name, obs)
		if err != nil {
			return nil, err
		}

		// New versions BELOW the head are still new work: the head comparison
		// alone would file them into history and trigger nothing.
		obs.dirty = obs.dirty || grew

		observed[name] = obs
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
	released, err := releaseConstrainedJobs(ctx, cfg, st, enqueued)
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
func enqueueAffected(ctx context.Context, cfg *config.Config, st PollStore, observed map[string]observedResource) ([]string, error) {
	reasons, err := affectedJobs(ctx, cfg, st, observed)
	if err != nil {
		return nil, err
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

			ready, err := jobReadyFor(ctx, st, job)
			if err != nil {
				return nil, err
			}

			if !ready {
				continue
			}

			reasons[job.Name] = resourceName
		}
	}

	return reasons, nil
}

// releaseConstrainedJobs enqueues every passed:-constrained job for which
// some version has now gone green upstream that the job has not itself run.
//
// The candidate is the newest GREEN version, not the newest version: those
// differ exactly when the head keeps failing upstream, and judging only the
// head starved the downstream job forever while a validated older version
// sat in history. Concourse selects the latest version satisfying the
// constraint, which is what asking the store — rather than this poll's
// observation — amounts to.
//
// The guard against enqueueing the same work forever is the job's OWN passed
// record: once it succeeds against a version it has recorded one, so it stops
// being releasable for that version. A job that keeps failing is re-attempted
// one at a time (the queue holds at most one pending row per job) until the
// circuit breaker pauses it, which is what the breaker is for.
func releaseConstrainedJobs(
	ctx context.Context, cfg *config.Config, st PollStore, alreadyEnqueued []string,
) ([]string, error) {
	var released []string

	for i := range cfg.Jobs {
		job := &cfg.Jobs[i]

		constraints := job.PassedConstraints()
		if len(constraints) == 0 || slices.Contains(alreadyEnqueued, job.Name) {
			continue
		}

		ready, err := jobReadyFor(ctx, st, job)
		if err != nil {
			return nil, err
		}

		if !ready {
			continue
		}

		done, err := jobAlreadyRanThese(ctx, st, job.Name, constraints)
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
// against every constrained resource's candidate version — the fact that
// stops a released job being released again on the next poll.
func jobAlreadyRanThese(
	ctx context.Context, st PollStore, jobName string,
	constraints map[string][]string,
) (bool, error) {
	want, complete, err := candidateSetFor(ctx, st, constraints)
	if err != nil {
		return false, err
	}

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
// satisfied by some recorded version — the newest green one per resource.
//
// This is the correctness gap passed: exists to close: without it, watch can
// trigger `deploy` on a commit the `test` job ALREADY FAILED on, and there is
// no way to say "don't deploy unless the tests were green for this exact
// commit".
//
// A job held back is not an error and not a lost trigger: the green record
// is durable, so the poll after the upstream job goes green enqueues it —
// even if newer, unproven versions have arrived on top in the meantime.
func jobReadyFor(ctx context.Context, st PollStore, job *config.Job) (bool, error) {
	// Inverted from resource -> upstream jobs into upstream job -> the set of
	// constrained resources naming it. That inversion IS the fix: each
	// upstream job is asked once, about every version it vouches for at once,
	// so a combination that passed only in pieces is refused. Asking per
	// resource could never see the combination at all.
	constraints := job.PassedConstraints()

	for _, upstreamJob := range upstreamJobsOf(job) {
		constrained := map[string][]string{}

		for resource, upstream := range constraints {
			if slices.Contains(upstream, upstreamJob) {
				constrained[resource] = upstream
			}
		}

		want, complete, err := candidateSetFor(ctx, st, constrained)
		if err != nil {
			return false, err
		}

		if !complete {
			// A constrained resource has no green version at all, so there is
			// nothing to judge. Holding the job back is the conservative
			// reading, and the one that matches "only run against a version
			// that passed".
			return false, nil
		}

		passed, err := pipeline.VersionSetPassedUpstream(ctx, st, upstreamJob, want)
		if err != nil {
			return false, fmt.Errorf("passed: constraint for job %q: %w", job.Name, err)
		}

		if !passed {
			names := slices.Sorted(maps.Keys(want))

			printf("trigger: %s waiting — %s has not gone green against this combination of %v yet\n", job.Name, upstreamJob, names)
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

// candidateSetFor picks, for every constrained resource, the version a
// released job would actually build: the newest one green in all of that
// resource's upstream jobs. complete is false when any resource has none —
// there is then nothing to release for.
//
// Reading the store rather than this poll's observation is the point. The
// observed head is one version, and judging only it meant a head that kept
// failing upstream starved the job forever while a validated version sat in
// history. It also kept the gate honest across the enqueue/claim window:
// the build resolves green versions again at claim time (see
// pipeline.loadResourceHistory), so what is judged here and what is built
// there come from the same durable record rather than a moment that has
// passed.
func candidateSetFor(
	ctx context.Context, st PollStore, constraints map[string][]string,
) (want map[string]map[string]any, complete bool, err error) {
	want = make(map[string]map[string]any, len(constraints))

	for resource, upstreams := range constraints {
		green, err := st.GreenVersions(ctx, resource, upstreams)
		if err != nil {
			return nil, false, fmt.Errorf("passed: constraint on %q: %w", resource, err)
		}

		if len(green) == 0 {
			return nil, false, nil
		}

		want[resource] = green[len(green)-1]
	}

	return want, true, nil
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

	ctx, err = pipeline.PlaceResource(ctx, cfg, resourceName)
	if err != nil {
		return observedResource{}, false, fmt.Errorf("trigger resource %q: %w", resourceName, err)
	}

	versions, err := rsrc.CheckVersions(ctx, cfg, *resourceType, resource.Env, resource.Source, cursor)
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

// recordedVersion reads the version last recorded for a resource, decoded.
// It is the check cursor and, when a check reports nothing new, still the
// resource's current version.
func recordedVersion(
	ctx context.Context, st PollStore, resourceName string,
) (encoded string, version map[string]any, found bool, err error) {
	last, found, err := st.LastChecked(ctx, resourceName)
	if err != nil {
		return "", nil, false, fmt.Errorf("trigger resource %q: %w", resourceName, err)
	}

	if !found {
		return "", nil, false, nil
	}

	encoded = last.Version

	version, err = rsrc.ParseVersionJSON(encoded)
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
	// a first watch after deleting the state db is exactly when someone is
	// staring at the log deciding whether to ^C.
	slog.Info("trigger.cold_start", "resource", resourceName, "versions_recorded", len(obs.versions),
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
func seedColdStart(ctx context.Context, cfg *config.Config, st PollStore, resourceName, latest string) error {
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
// indistinguishable from a watcher that does not work. Concourse builds the
// version its first check reports; this is the same outcome for a check that
// reports a window rather than only its current version.
//
// The mark is a high-water mark over DISCOVERY order, so this is the largest
// order strictly below the newest's. Orders are not assumed contiguous
// (RecordVersionOrder assigns MAX+1, and a pin can mint one), hence a scan
// rather than "highest - 1". A latest that is somehow not the highest order
// falls back to seeding everything: refusing to build is the safe direction
// when the ordering says something this function does not understand.
func coldStartMark(orders map[string]int64, latest string) int64 {
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

// leasedChecks scopes one round of checks the way RunJob scopes a job, so a placed check resolves its worker through the same registry a job's steps do — which is what lets a poll end without stopping a machine a job is on.
func leasedChecks(ctx context.Context) (context.Context, func()) {
	ctx, releaseWorkers := pipeline.WithLeases(ctx)

	return pipeline.WithResourcePlacement(ctx), func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pipeline.WorkerReleaseTimeout)
		defer cancel()

		releaseWorkers(releaseCtx)
	}
}
