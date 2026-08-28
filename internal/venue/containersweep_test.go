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
		mu    sync.Mutex
		swept []string
	)

	original := sweepContainers

	t.Cleanup(func() { sweepContainers = original })

	sweepContainers = func(_ context.Context, dockerHost string) {
		mu.Lock()
		defer mu.Unlock()

		swept = append(swept, dockerHost)
	}

	spec := localWorker(t, t.TempDir())
	spec.Image = "alpine"

	runner := newLocalRunner(t, spec)

	// Ignored: a bind mount the daemon cannot see fails the container on some
	// hosts, and this test is about the call that already happened by then.
	_ = runner.Run(context.Background(), "true")

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
}
