package venue

// What every cloud's acquisition ladder shares: the rungs, the bounds on
// waiting for a machine and on giving one back, and the static worker an
// acquired machine becomes. ec2.go and gce.go each speak one cloud's API.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/jtarchie/steps/internal/events"
)

// Rung is how much has to happen before a worker can be dialed.
type Rung string

const (
	// RungStatic is a machine that is already running.
	RungStatic Rung = ""
	// RungStopped is a parked machine: started for the job, stopped after.
	RungStopped Rung = "stopped"
	// RungLaunch is a machine born for the job and terminated with it.
	RungLaunch Rung = "launch"
)

// Capacity says which EC2 capacity to ask for on the launch rung.
type Capacity string

const (
	// CapacitySpot asks for spot only, and fails when there is none.
	CapacitySpot Capacity = "spot"
	// CapacitySpotThenOD asks for spot and falls back to on-demand in the
	// same call, so a job runs rather than failing on a busy pool.
	CapacitySpotThenOD Capacity = "spot-then-od"
	// CapacityOnDemand asks for on-demand only.
	CapacityOnDemand Capacity = "od"
)

// defaultIdle is zero because a warm machine is a billed one: ?idle= is the operator choosing to pay to spare the next user a cold start.
const defaultIdle = 0

// acquireTimeout bounds waiting for a machine to reach a usable state. Cloud
// acquisition is 20-90 seconds; a Windows instance without fast launch is
// minutes, which is one more reason Windows is refused earlier.
// acquirePoll is how often a starting instance is asked whether it is ready.
//
// Variables rather than constants so a test can shrink them, exactly as the
// SSM registration and GCP boot waits are: every branch here is reached by
// polling at least twice, so against a fake the constants bought nothing but
// a real five-second sleep per test.
//
//nolint:gochecknoglobals // test seams for a wait measured in minutes
var (
	acquireTimeout = 10 * time.Minute
	acquirePoll    = 5 * time.Second
)

// needsAcquisition reports whether a worker names a machine that has to be
// brought into existence before it can be dialed.
func (w Worker) needsAcquisition() bool {
	return w.Rung == RungStopped || w.Rung == RungLaunch
}

// acquire brings a worker's machine into existence and returns the static
// worker that names it, plus how to give it back. A scheme switch rather
// than an if-chain, so the exhaustive linter makes the next scheme decide
// its acquisition story instead of falling into another cloud's.
func acquire(ctx context.Context, worker Worker) (Worker, func(context.Context) error, error) {
	switch worker.Scheme {
	case SchemeAWS:
		return acquireEC2(ctx, worker)
	case SchemeGCP:
		return acquireGCE(ctx, worker)
	case SchemeLocal, SchemeSSH, SchemeDockerSSH:
		// Machines that already exist by definition; nothing to acquire.
		return worker, nil, nil
	default:
		return worker, nil, nil
	}
}

// ownedKey marks an acquisition of a machine this process already owes a park, so finding it running is no news: it is steps' own earlier start.
type ownedKey struct{}

func withOwned(ctx context.Context) context.Context { return context.WithValue(ctx, ownedKey{}, true) }

// noteAdopted warns every time, because nothing records what a crashed or restarted steps started (#120): each such machine is found running here, and bills until somebody reads this.
func noteAdopted(ctx context.Context, worker Worker) {
	if owned, _ := ctx.Value(ownedKey{}).(bool); owned {
		events.Note(ctx, events.NoteInfo, fmt.Sprintf("worker %s: %s is still running from steps' own earlier start; it will be parked when its last user is done", worker.URL, worker.Instance))

		return
	}

	events.Note(ctx, events.NoteWarn, fmt.Sprintf("worker %s: %s was already running, so steps did not start it and will not stop it — if a steps process that crashed or restarted started it, it is still billing; stop it yourself", worker.URL, worker.Instance))
	slog.WarnContext(ctx, "worker.adopted", "worker", worker.URL, "instance", worker.Instance)
}

// cleanupTimeout bounds the API call that gives a machine back on a path
// whose own context may be spent or cancelled — which is exactly when leaving
// the machine running would be worst.
const cleanupTimeout = 2 * time.Minute

// giveBack runs one give-back call under its own bounded context, for the
// reason cleanupTimeout gives: an aborted Stop is a machine left billing.
func giveBack(ctx context.Context, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	return fn(ctx)
}

// cleanupRelease is a lease's release for an acquired machine: verb names
// the call in its error ("stopping"), done the outcome in the note ("parked").
func cleanupRelease(worker Worker, instance, verb, done string, fn func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		err := giveBack(ctx, fn)
		if err != nil {
			return fmt.Errorf("%s %s for %q: %w", verb, instance, worker.URL, err)
		}

		events.Note(ctx, events.NoteInfo, fmt.Sprintf("worker %s: %s %s", worker.URL, done, instance))

		return nil
	}
}

// cleanupAfterFailure is the best-effort give-back for a machine a failed
// acquisition leaves running. No caller is left to report to, so a failure is
// logged: the line is how a machine that still bills gets found.
func cleanupAfterFailure(worker Worker, instance, event string, fn func(context.Context) error) {
	err := giveBack(context.Background(), fn)
	if err != nil {
		slog.Warn(event, "instance", instance, "worker", worker.URL, "after", "a failed acquisition", "error", err)
	}
}

// pollUntil calls step every interval until it reports done or an error,
// bounded by timeout on top of ctx. Its own expiry comes back as a
// pollTimeoutError, which waitingFor names; step's errors come back as-is.
func pollUntil(ctx context.Context, timeout, every time.Duration, step func(context.Context) (bool, error)) error {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		done, err := step(deadline)
		if done || err != nil {
			return err
		}

		select {
		case <-ticker.C:
		case <-deadline.Done():
			return pollTimeoutError{cause: deadline.Err()}
		}
	}
}

// pollTimeoutError is pollUntil running out of time rather than step failing.
type pollTimeoutError struct{ cause error }

func (e pollTimeoutError) Error() string { return e.cause.Error() }
func (e pollTimeoutError) Unwrap() error { return e.cause }

// waitingFor names the machine a timed-out poll was waiting on, leaving every
// other error as the step already worded it.
func waitingFor(err error, worker Worker, instance string) error {
	var expired pollTimeoutError
	if errors.As(err, &expired) {
		return fmt.Errorf("waiting for %s for %q: %w", instance, worker.URL, expired.cause)
	}

	return err
}

// asStatic is this worker as the already-running machine it just became, so
// everything downstream dials it without knowing how it got there.
//
// The URL is REBUILT, and that is the load-bearing part: the pipeline hands a
// runner the worker's URL as a string, and the venue parses it back — so a
// launch-rung worker whose URL still said aws://launch/lt-… would re-parse
// into a template with no instance, and the session would dial nothing at
// all, on a machine the job had just paid to start. The acquisition-only
// options are stripped because a static parse refuses them, by design.
func (w Worker) asStatic(instance string) Worker {
	w.Rung = RungStatic
	w.Instance = instance
	w.URL = staticURL(w.Scheme, instance, w.Root, w.Query)

	return w
}

// staticURL names a running instance the way ParseWorker reads one, keeping
// every connection option and dropping the acquisition ones.
func staticURL(scheme Scheme, instance, root, rawQuery string) string {
	// Escaped, because root is the DECODED path: a %3F in the mapping comes back as a ? that would start the query.
	address := string(scheme) + "://" + instance + (&url.URL{Path: root}).EscapedPath()

	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return address
	}

	for _, key := range acquisitionKeys {
		query.Del(key)
	}

	if len(query) == 0 {
		return address
	}

	return address + "?" + query.Encode()
}
