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
	"log/slog"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/venue"
)

// newRunner is venue.NewRunner, swapped by tests for a worker the cloud reclaims mid-command, which no local machine can stage.
var newRunner = venue.NewRunner //nolint:gochecknoglobals // test seam

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

// claimWorker takes the job's place on a tag's machine, waiting its turn on a capped exclusive pool, without acquiring it: called before a step's timeout: starts, because the wait has no bound of its own and is not the step's.
func claimWorker(ctx context.Context, tag string) error {
	leases := leasesFrom(ctx)
	if tag == "" || leases == nil {
		return nil
	}

	err := leases.Claim(ctx, tag)
	if err != nil {
		return fmt.Errorf("worker for tag %q: %w", tag, err)
	}

	return nil
}

// reusedWarm is a step whose machine this job took from an idle window, which the venue treats as reclaimed when it will not answer — see shell.RunnerSpec.ReusedWarm.
func reusedWarm(ctx context.Context, step config.Step) bool {
	leases := leasesFrom(ctx)

	return leases != nil && leases.ReusedWarm(placementTag(step))
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

	// Before the clock starts: a turn waited on a capped pool is not the step's to pay for.
	err := claimWorker(ctx, tag)
	if err != nil {
		return err
	}

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

		notef(ctx, "worker for tag %s was reclaimed; re-placing the step", tag)
		slog.InfoContext(ctx, "job.worker_evicted", "tag", tag, "attempt", attempt+1, "error", err)
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
	case !acquiredOnDemand(ctx, tag):
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

// acquiredOnDemand reports whether a tag names something a fresh machine can be
// acquired for. A worker that already exists has nowhere else to go.
func acquiredOnDemand(ctx context.Context, tag string) bool {
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
	tag := placementTag(step)

	leases := leasesFrom(ctx)
	if tag == "" || leases == nil {
		return
	}

	// A warm reuse is presumed dead only until it answers; after that, a later step's refused dial is its own failure, not a machine that died while kept.
	if _, answered := venue.PlacementOf(runner); answered {
		leases.Answered(tag)
	}

	reason, reclaimed := venue.ReclaimedBy(runner)
	if !reclaimed {
		return
	}

	notef(ctx, "worker for tag %s finished the step and is being reclaimed; letting it go", tag)
	slog.InfoContext(ctx, "job.worker_abandoned_after_drain", "tag", tag, "reason", reason)

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
