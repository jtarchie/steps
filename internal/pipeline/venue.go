package pipeline

// Turning a step's tags: into the machine it runs on.
//
// The pipeline says what a step needs; the invocation says which machine has
// it. Keeping the mapping out of the pipeline file is what lets the same
// pipeline run on somebody else's fleet, and it is the same split Concourse
// draws between a step's tags: and a worker's advertised ones.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/venue"
)

// workersKey types the context value carrying the tag-to-worker mapping.
type workersKey struct{}

// WithWorkers records the --worker mappings for this invocation, rejecting any
// that cannot be reached as written.
//
// Parsed here rather than at first use so a typo in a worker URL is reported
// before a run starts, alongside every other thing that is wrong with the
// invocation, rather than mid-plan when a step happens to reach for it.
func WithWorkers(ctx context.Context, mappings map[string]string) (context.Context, error) {
	if len(mappings) == 0 {
		return ctx, nil
	}

	workers := make(map[string]venue.Worker, len(mappings))

	for _, tag := range sortedKeys(mappings) {
		worker, err := venue.ParseWorker(mappings[tag])
		if err != nil {
			return nil, fmt.Errorf("--worker %s: %w", tag, err)
		}

		workers[tag] = worker
	}

	return context.WithValue(ctx, workersKey{}, workers), nil
}

func workersFrom(ctx context.Context) map[string]venue.Worker {
	workers, _ := ctx.Value(workersKey{}).(map[string]venue.Worker)

	return workers
}

// artifactStoreKey types the context value carrying the --artifact-store URL.
type artifactStoreKey struct{}

// WithArtifactStore records the --artifact-store URL so a placed step's venue
// can offer the worker the URL data plane. The URL was already parsed at the
// CLI edge; this carries the fact, not a client.
func WithArtifactStore(ctx context.Context, raw string) context.Context {
	if raw == "" {
		return ctx
	}

	return context.WithValue(ctx, artifactStoreKey{}, raw)
}

func artifactStoreFrom(ctx context.Context) string {
	raw, _ := ctx.Value(artifactStoreKey{}).(string)

	return raw
}

// leasesKey types the context value carrying one job's venue leases.
type leasesKey struct{}

// WithLeases installs a job's venue leases and returns the release that has
// to run when the job ends.
//
// Per JOB, not per run and not per step. A worker that has to be acquired —
// started from stopped, or launched outright — costs 20 to 90 seconds and
// real money, so the first placed step pays for it, every later step in the
// job reuses it, and the job's end gives it back. A job with no placed step,
// or whose placed steps are all cache hits, acquires nothing at all.
func WithLeases(ctx context.Context) (context.Context, func(context.Context)) {
	leases := venue.NewLeases(workersFrom(ctx))

	return context.WithValue(ctx, leasesKey{}, leases), func(ctx context.Context) {
		err := leases.ReleaseAll(ctx)
		if err != nil {
			// Logged, never returned: a job that succeeded did succeed, and
			// a machine that could not be given back is an operational
			// problem rather than a wrong answer. It is loud because it
			// costs money for as long as nobody notices.
			logFrom(ctx).Error("job.worker_release_failed", "error", err)
			fmt.Printf("warning: a worker acquired for this job could not be released: %v\n", err)
		}
	}
}

func leasesFrom(ctx context.Context) *venue.Leases {
	leases, _ := ctx.Value(leasesKey{}).(*venue.Leases)

	return leases
}

// workerFor answers which worker a step runs on, or "" for this machine,
// acquiring the machine if this is the first step in the job to ask.
func workerFor(ctx context.Context, step config.Step) (string, error) {
	tag := placementTag(step)
	if tag == "" {
		return "", nil
	}

	leases := leasesFrom(ctx)
	if leases == nil {
		// A caller that never installed leases — every test that builds a
		// step runner directly. An already-running worker needs none of it.
		worker, ok := workersFrom(ctx)[tag]
		if !ok {
			return "", nil
		}

		return worker.URL, nil
	}

	worker, err := leases.Resolve(ctx, tag)
	if err != nil {
		return "", fmt.Errorf("worker for tag %q: %w", tag, err)
	}

	return worker.URL, nil
}

// venueRetries is how many times a step may be re-placed after its worker was
// taken away, on top of whatever attempts: the author set.
//
// A DELIBERATE divergence from Concourse, which errors the build when a
// worker vanishes. An eviction is not the step failing and not the step being
// flaky: attempts: is the author's statement about their own work, and
// spending it on the cloud reclaiming a spot instance would bill them for
// something they neither caused nor can fix. Two, not unlimited, because a
// pool that keeps reclaiming is a capacity problem the build should surface
// rather than grind against.
const venueRetries = 2

// withVenueRetry runs a placed step's work, re-acquiring its worker and
// starting over when the machine is taken away underneath it.
//
// Outside the attempts: loop rather than inside, which is what makes the
// budgets independent: an eviction rewinds to a fresh machine with the step's
// full attempts: budget intact, and a step that genuinely fails spends only
// its own. The step's own wall clock still bounds the WHOLE of it — see
// budget — because attempts: and timeout: are a contract with the author
// (docs/attempts-timeout.md) and re-placement must not quietly multiply it.
//
// Re-placed only when there is a fresh machine to be had. A tag naming a
// machine that already exists — every ssh:// worker, a static aws://i-* —
// resolves to the same address next time round, so retrying would re-run the
// step against the host that just went away, paying a dial timeout and any
// side effects the commands already had, twice, to reach the same end.
// work reports the dial URL of the machine it ran against alongside its
// error, because forgetting a lease is identity-checked: the machine to
// forget is the one THIS attempt watched die, never whatever the tag happens
// to hold by then — a parallel sibling may have re-acquired already.
func withVenueRetry(ctx context.Context, step config.Step, budget time.Duration, work func(context.Context) (string, error)) error {
	tag := placementTag(step)

	// Enforced by refusing to START a late attempt rather than by bounding
	// the context: a deadline on the parent would expire at the same instant
	// as the last attempt's own, and retryWithTimeout reads a live parent as
	// what separates "the step overran its budget" (a failure) from "the job
	// was aborted" (an error). Cancelling here would relabel every timed-out
	// placed step.
	started := time.Now()

	for attempt := 0; ; attempt++ {
		dialed, err := work(ctx)
		if err == nil || !errors.Is(err, venue.ErrEvicted) {
			return err
		}

		// A build being torn down must not acquire anything: without this, a
		// Ctrl-C on a drained session reads as an eviction and launches a
		// fresh instance for a job the user already stopped.
		if ctx.Err() != nil {
			return err
		}

		stop := replacementRefusal(ctx, tag, attempt, budget, started)
		if stop != "" {
			return fmt.Errorf("%w (%s)", err, stop)
		}

		// Forgotten, never destroyed: AWS is already reclaiming the machine,
		// and a sibling step may still be finishing its own work inside the
		// two-minute grace. The next resolve acquires a fresh one.
		if leases := leasesFrom(ctx); leases != nil {
			leases.Abandon(tag, dialed)
		}

		fmt.Printf("worker for tag %s was reclaimed; re-placing the step\n", tag)
		logFrom(ctx).Info("job.worker_evicted", "tag", tag, "attempt", attempt+1, "error", err)
	}
}

// replacementRefusal says why this step will not be re-placed, or empty when
// it will be.
func replacementRefusal(ctx context.Context, tag string, attempt int, budget time.Duration, started time.Time) string {
	switch {
	case attempt >= venueRetries:
		return fmt.Sprintf("after %d re-placements", attempt)
	case tag == "":
		return "the step names no worker"
	case !canReplace(ctx, tag):
		// Nothing to acquire: the tag names a machine that already exists, so
		// the next resolve would hand back the same address and the step
		// would re-run against the host that just went away.
		return "its worker names a machine that already exists, so there is no fresh one to take"
	case budget > 0 && time.Since(started) >= budget:
		return fmt.Sprintf("the step's own time budget was spent after %d re-placements", attempt)
	default:
		return ""
	}
}

// canReplace reports whether a tag names something a fresh machine can be
// acquired for. A worker that already exists has nowhere else to go.
func canReplace(ctx context.Context, tag string) bool {
	worker, ok := workersFrom(ctx)[tag]

	return ok && worker.Acquirable()
}

// releaseIfReclaimed forgets a worker that announced its own end, after a
// step that finished anyway.
//
// The two-minute window means a step often SUCCEEDS on a machine that is
// going away, and nothing about that success says so. Without this the lease
// stays bound to the doomed instance and the next step on the tag opens a
// session against a host that dies at the handshake — which sticks, is not an
// eviction, and fails the build on a machine steps already knew was dying.
func releaseIfReclaimed(ctx context.Context, step config.Step, runner shell.Runner, dialed string) {
	reason, reclaimed := venue.ReclaimedBy(runner)
	if !reclaimed {
		return
	}

	tag := placementTag(step)
	if tag == "" {
		return
	}

	leases := leasesFrom(ctx)
	if leases == nil {
		return
	}

	fmt.Printf("worker for tag %s finished the step and is being reclaimed; letting it go\n", tag)
	logFrom(ctx).Info("job.worker_abandoned_after_drain", "tag", tag, "reason", reason)

	// Forgotten, never destroyed — AWS owns this machine's end, and a
	// sibling step may still be using its remaining grace. See Abandon.
	leases.Abandon(tag, dialed)
}

// placementOf describes where a step ran, for the run record: "tag (address)",
// or empty for a step that ran on this machine.
//
// Absence is the signal, so this answers empty for an untagged step rather
// than naming the local machine — see events.Event.Worker. The address rather
// than the mapping as written, so ?identity= and ?hostkey= stay out of a
// record that gets drawn in a browser.
func placementOf(ctx context.Context, step config.Step) string {
	tag := placementTag(step)
	if tag == "" {
		return ""
	}

	worker, ok := workersFrom(ctx)[tag]
	if !ok {
		// Unreachable for the reason workerFor says; the tag alone is still
		// the honest answer if it ever happens.
		return tag
	}

	return tag + " (" + worker.Address() + ")"
}

// placementTag is the one tag a step carries, if any. A try: wrapper is
// transparent here for the same reason it is everywhere else: the wrapped step
// is the one that runs.
func placementTag(step config.Step) string {
	if step.Try != nil {
		return placementTag(*step.Try)
	}

	if len(step.Tags) == 0 {
		return ""
	}

	return step.Tags[0]
}

// ValidateWorkerPlacement refuses a job whose steps name workers this
// invocation cannot supply.
//
// Before anything runs, and deliberately not a fallback to local execution: a
// step that says it needs a GPU box, quietly running on a laptop instead, is
// the kind of promise-it-cannot-keep that network: without image: is refused
// for. A pipeline that has to run without workers says so by not tagging.
func ValidateWorkerPlacement(ctx context.Context, cfg *config.Config, job *config.Job) error {
	workers := workersFrom(ctx)

	missing := map[string][]string{}

	err := job.VisitSteps(func(label string, step *config.Step) error {
		// The step's own tag, and — for a get or put — the RESOURCE's, which
		// is where its check runs even when the step overrides it for the
		// fetch. Both are dialled, so both have to be mapped.
		for _, tag := range stepPlacementTags(cfg, *step) {
			worker, ok := workers[tag]
			if !ok {
				missing[tag] = append(missing[tag], label)

				continue
			}

			// With what the invocation already knows: a dial certain to
			// fail is refused before any step runs — and before an
			// acquisition rung launches a billed machine to discover it.
			checkErr := worker.PlacementCheck(artifactStoreFrom(ctx) != "")
			if checkErr != nil {
				return fmt.Errorf("--worker %s: %w", tag, checkErr)
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("job %q: %w", job.Name, err)
	}

	if len(missing) == 0 {
		return nil
	}

	tags := sortedKeys(missing)

	return fmt.Errorf("job %q: no worker is registered for %s (%s) — map it with --worker %s=ssh://user@host, or remove the tag",
		job.Name, pluralTag(len(tags)), describeMissing(missing, tags), tags[0])
}

func describeMissing(missing map[string][]string, tags []string) string {
	parts := make([]string, 0, len(tags))

	for _, tag := range tags {
		parts = append(parts, fmt.Sprintf("%s on %s", tag, strings.Join(missing[tag], ", ")))
	}

	return strings.Join(parts, "; ")
}

func pluralTag(n int) string {
	if n == 1 {
		return "tag"
	}

	return "tags"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

// placementSinkKey types the context value carrying one step's placement
// facts.
type placementSinkKey struct{}

// placementSink is where a step leaves what its worker said about itself, for
// the enclosing step to record once the node that row references exists.
//
// It is a handoff and not a return value because the facts are only whole at
// a point nothing on the call path can see: a session dials LAZILY, so the
// workdir and filesystem arrive with the first command, and the byte count is
// final only when the runner closes. The one place that knows all of it is a
// defer inside the venue-retry closure, several frames below the caller that
// has the node hash.
//
// Last write wins, which is the answer a re-placed step wants: a step evicted
// off one machine and finished on another ran on the second, and the first
// is already reported as an eviction.
type placementSink struct {
	mu    sync.Mutex
	last  venue.Placement
	noted bool
}

func (s *placementSink) note(placement venue.Placement) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.last, s.noted = placement, true
}

func (s *placementSink) taken() (venue.Placement, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.last, s.noted
}

// withPlacementSink gives one step somewhere to leave its placement facts.
//
// Installed per STEP rather than per job: the sink is keyed by nothing, so a
// job-wide one would have parallel steps overwriting each other's machine.
func withPlacementSink(ctx context.Context) (context.Context, *placementSink) {
	sink := &placementSink{}

	return context.WithValue(ctx, placementSinkKey{}, sink), sink
}

// notePlacement records what a finished runner's worker said about itself.
//
// Deferred rather than called after the runner is built, because a session
// that has not been asked to run anything has not dialed and has nothing to
// say — and because BytesSent is only whole once the step is done with it.
func notePlacement(ctx context.Context, runner shell.Runner) {
	sink, _ := ctx.Value(placementSinkKey{}).(*placementSink)
	if sink == nil {
		return
	}

	placement, ok := venue.PlacementOf(runner)
	if !ok {
		return
	}

	sink.note(placement)
}

// recordPlacement persists where a step ran, if it ran anywhere but here.
//
// Best-effort, like the agent's usage row and for the same reason: a
// bookkeeping write must never turn a step that did its work into a failed
// one. For a step that HAS a node, called only after that node is recorded:
// the foreign key that lets retention reap the two together also means the
// row cannot be written first.
//
// slot is what identifies the row within the run: a plan step's node hash, or
// a hook's scope label. node is that hash again when the step HAS one and
// EMPTY when it does not — a hook is deliberately not merkle-hashed, so it has
// no node for retention to reap the row alongside, and cascades off its run
// instead.
func recordPlacement(ctx context.Context, runner stepRunner, sink *placementSink, index int, name, slot, node string) {
	if runner.st == nil {
		return
	}

	placement, ok := sink.taken()
	if !ok {
		return
	}

	runID := events.RunID(ctx)
	if runID == "" {
		return
	}

	var instance *string
	if placement.Instance != "" {
		instance = &placement.Instance
	}

	// Detached: the likeliest reason a placed step is finishing is that it was
	// cancelled or timed out, and those are precisely the runs whose machine
	// somebody wants named.
	err := runner.st.RecordPlacement(context.WithoutCancel(ctx), store.Placement{
		RunID:      runID,
		StepIndex:  index,
		StepName:   name,
		JobName:    runner.jobName,
		Slot:       slot,
		NodeHash:   node,
		Tag:        placement.Tag,
		Address:    placement.Address,
		InstanceID: instance,
		GOOS:       placement.GOOS,
		GOARCH:     placement.GOARCH,
		Workdir:    placement.Workdir,
		FSType:     placement.FSType,
		//nolint:gosec // a filesystem's free bytes does not reach 2^63
		FSFree:    int64(placement.FSFree),
		UID:       placement.UID,
		GID:       placement.GID,
		Image:     placement.Image,
		BytesSent: placement.BytesSent,
	})
	if err != nil {
		logFrom(ctx).Warn("job.placement_unrecorded", "job", runner.jobName, "step", name, "error", err)
	}
}
