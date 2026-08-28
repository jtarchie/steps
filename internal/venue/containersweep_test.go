package venue

// Which daemon gets swept.

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// TestWorkerSweepTargetsTheWorkerDaemon is the seam, and the seam is the whole
// bug.
//
// Both halves were already right and already tested: shell knows how to sweep
// a daemon, and the venue knows how to forward a socket to the worker's. What
// never happened was the socket crossing between them — the sweep only ever
// ran against the ORCHESTRATOR's daemon, so a container a placed step could
// not tear down kept running on a worker nothing ever looked at. The same
// shape as the mount path and the user before it, and the same reason a
// local: worker cannot notice: there both ends are one daemon.
//
// The container is not required to start. The sweep happens BEFORE the runner
// is built — reclaiming the machine is the point of doing it first — so what
// is asserted here is which daemon was asked, not whether the step then ran.
func TestWorkerSweepTargetsTheWorkerDaemon(t *testing.T) {
	requireDockerVenue(t)

	var (
		mu      sync.Mutex
		swept   []string
		routed  []bool
		session *session
	)

	original := sweepContainers

	t.Cleanup(func() { sweepContainers = original })

	sweepContainers = func(_ context.Context, dockerHost string) {
		mu.Lock()
		defer mu.Unlock()

		swept = append(swept, dockerHost)

		// The OTHER half of the same seam, and the half a socket string
		// cannot show. The forwarded socket only answers while the relay's
		// router owns the wire — it is the one goroutine that carries the
		// daemon's replies back — so a sweep issued outside a routing bracket
		// is a `docker ps` nothing ever answers: it waited out the full
		// dockerSweepTimeout, under the session mutex, on every placed
		// containerized step, and listed nothing.
		relay := session.relay.Load()
		routed = append(routed, relay != nil && relay.routing.Load())
	}

	spec := localWorker(t, t.TempDir())
	spec.Image = "alpine"

	placed := newLocalRunner(t, spec)

	venued, ok := placed.(runner)
	if !ok {
		t.Fatalf("NewRunner returned %T, want a venue runner", placed)
	}

	session = venued.session

	// Ignored: a bind mount the daemon cannot see fails the container on some
	// hosts, and this test is about the call that already happened by then.
	_ = placed.Run(context.Background(), "true")

	mu.Lock()
	defer mu.Unlock()

	if len(swept) == 0 {
		t.Fatal("no daemon was swept — a worker's leftover containers are reclaimed by nothing")
	}

	// Never the empty string: empty is the ORCHESTRATOR's own daemon, the one
	// machine that was already being swept and the one not holding the leak.
	if swept[0] == "" {
		t.Fatal("the sweep was pointed at the local daemon rather than the worker's")
	}

	if !strings.HasPrefix(swept[0], "unix://") || !strings.HasSuffix(swept[0], ".sock") {
		t.Errorf("swept %q, want the forwarded socket the container runner is given", swept[0])
	}

	if !routed[0] {
		t.Error("the sweep ran with no router owning the wire — its docker ps can never be answered")
	}
}
