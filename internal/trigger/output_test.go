package trigger

// Reading what a watch printed, while it is still printing.

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestCaptureDoesNotRaceConcurrentOutput is the deterministic form of a flake.
//
// The capture helper used to assign the os.Stdout GLOBAL while this package's
// parallel tests read the same variable through every fmt.Printf in the code
// under test. -race caught it intermittently — four failures in one full-suite
// run, unreproducible in three clean ones — which is the worst shape a
// failure can have: it reads as infrastructure noise, so it gets re-run rather
// than read.
//
// Written as one capture against a concurrent printer so the race is present
// on every run rather than under load, and so -race has something to say
// about it without a hundred repetitions.
func TestCaptureDoesNotRaceConcurrentOutput(t *testing.T) {
	t.Parallel()

	var wg sync.WaitGroup

	stop := make(chan struct{})

	wg.Add(1)

	go func() {
		defer wg.Done()

		for {
			select {
			case <-stop:
				return
			default:
				printf("trigger: a line from somewhere else\n")
			}
		}
	}()

	captured := captureStdout(t, func() {
		printf("trigger: enqueued %s\n", "the-one-under-test")
	})

	close(stop)
	wg.Wait()

	// The line asked for is there. Whether a neighbour's line landed beside it
	// is not asserted: two writers to one destination interleave, which is
	// true of a terminal too, and is not what this is about.
	if !strings.Contains(captured, "the-one-under-test") {
		t.Errorf("the capture missed its own line: %q", captured)
	}
}

// TestConcurrentCapturesEachKeepTheirOwnLine is the other half of the
// contract, and the half that was missing.
//
// TestCaptureDoesNotRaceConcurrentOutput proves a capture survives a
// concurrent PRINTER. It says nothing about a concurrent CAPTURE, and there is
// only one destination to swap: two captures in flight at once both redirect
// `out`, the later swap wins, and the earlier test's own line is delivered
// into the later test's pipe. The earlier test then fails reporting that it
// never printed something it did print — which is how
// TestDrainOneSkipsAPausedJob failed under a full-suite run while passing
// alone every time.
//
// Both halves are asserted, because losing either is a real regression: each
// capture must contain its OWN line, and must not be the only place a
// neighbour's line could have gone.
func TestConcurrentCapturesEachKeepTheirOwnLine(t *testing.T) {
	t.Parallel()

	const captures = 8

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		captured = map[string]string{}
	)

	for i := range captures {
		wg.Add(1)

		go func() {
			defer wg.Done()

			mine := "line-for-capture-" + strconv.Itoa(i)

			got := captureStdout(t, func() {
				printf("trigger: %s\n", mine)
			})

			mu.Lock()
			defer mu.Unlock()

			captured[mine] = got
		}()
	}

	wg.Wait()

	for mine, got := range captured {
		if !strings.Contains(got, mine) {
			t.Errorf("a capture lost its own line %q; it captured: %q", mine, got)
		}
	}
}
