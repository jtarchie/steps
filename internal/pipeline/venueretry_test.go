package pipeline

// The re-placement loop, which had no tests at all: the vacuity audit proved
// that deleting the budget refusal, the ctx check, or the retry.Stop wrap
// shipped green through the entire suite.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/venue"
)

// evictionCtx is a context whose tag maps to an acquisition-rung worker, so
// canReplace answers true and the loop's own refusals are what get exercised.
func evictionCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, err := WithWorkers(context.Background(), map[string]string{
		"gpu": "aws://stopped/i-0abc123def456789?idle=0",
	})
	if err != nil {
		t.Fatalf("WithWorkers: %v", err)
	}

	return ctx
}

func taggedStep() config.Step {
	return config.Step{Task: "work", Tags: []string{"gpu"}}
}

// evicted is the error shape the venue produces for a reclaimed machine.
var errTestEvicted = fmt.Errorf("%w (EC2 spot terminate): connection lost", venue.ErrEvicted)

// TestVenueRetryReplacesUpToTheCap pins the divergence's own bound: an
// eviction is re-placed, twice, and the third strike is reported rather than
// ground against.
func TestVenueRetryReplacesUpToTheCap(t *testing.T) {
	t.Parallel()

	runs := 0

	err := withVenueRetry(evictionCtx(t), taggedStep(), 0, func(context.Context) (string, error) {
		runs++

		return "aws://i-0abc123def456789", errTestEvicted
	})
	if err == nil {
		t.Fatal("a machine evicted every time reported success")
	}

	if runs != venueRetries+1 {
		t.Errorf("the work ran %d times, want the original and %d re-placements", runs, venueRetries)
	}

	if !errors.Is(err, venue.ErrEvicted) {
		t.Errorf("error = %v, want the eviction preserved", err)
	}
}

// TestVenueRetryStopsWhenTheBudgetIsSpent pins the wall-clock promise: a step
// whose attempts: x timeout: is already spent is not granted another
// machine's worth of it.
func TestVenueRetryStopsWhenTheBudgetIsSpent(t *testing.T) {
	t.Parallel()

	runs := 0

	err := withVenueRetry(evictionCtx(t), taggedStep(), time.Nanosecond, func(context.Context) (string, error) {
		runs++
		time.Sleep(2 * time.Millisecond)

		return "aws://i-0abc123def456789", errTestEvicted
	})
	if err == nil {
		t.Fatal("an evicted step past its budget reported success")
	}

	if runs != 1 {
		t.Errorf("the work ran %d times, want 1 — the budget was already spent", runs)
	}

	if !errors.Is(err, venue.ErrEvicted) {
		t.Errorf("error = %v, want the eviction preserved under the refusal", err)
	}
}

// TestVenueRetryStopsForACancelledBuild pins that a build being torn down
// acquires nothing: without the check, a Ctrl-C on a drained session read as
// an eviction and launched a fresh instance for a job the user had stopped.
func TestVenueRetryStopsForACancelledBuild(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(evictionCtx(t))

	runs := 0

	err := withVenueRetry(ctx, taggedStep(), 0, func(context.Context) (string, error) {
		runs++
		cancel()

		return "aws://i-0abc123def456789", errTestEvicted
	})
	if err == nil {
		t.Fatal("a cancelled build reported success")
	}

	if runs != 1 {
		t.Errorf("the work ran %d times after cancellation, want 1", runs)
	}
}

// TestVenueRetryReportsAWorkerWithNowhereElseToGo pins the acquired-only
// rule: a tag naming a machine that already exists resolves to the same
// address next time, so re-placing would re-run the step against the host
// that just vanished.
func TestVenueRetryReportsAWorkerWithNowhereElseToGo(t *testing.T) {
	t.Parallel()

	ctx, err := WithWorkers(context.Background(), map[string]string{
		"gpu": "ssh://jt@gpu-box",
	})
	if err != nil {
		t.Fatalf("WithWorkers: %v", err)
	}

	runs := 0

	err = withVenueRetry(ctx, taggedStep(), 0, func(context.Context) (string, error) {
		runs++

		return "ssh://jt@gpu-box", errTestEvicted
	})
	if err == nil {
		t.Fatal("an evicted static worker reported success")
	}

	if runs != 1 {
		t.Errorf("the work ran %d times, want 1 — there is no fresh machine to take", runs)
	}
}

// TestVenueRetryPassesOrdinaryOutcomesThrough pins that the loop is invisible
// to everything that is not an eviction: successes, failures and errors cross
// it untouched, exactly once.
func TestVenueRetryPassesOrdinaryOutcomesThrough(t *testing.T) {
	t.Parallel()

	for name, outcome := range map[string]error{"success": nil, "failure": errors.New("exit 1")} {
		runs := 0

		err := withVenueRetry(evictionCtx(t), taggedStep(), 0, func(context.Context) (string, error) {
			runs++

			return "aws://i-0abc123def456789", outcome
		})
		if !errors.Is(err, outcome) && (err != nil) != (outcome != nil) {
			t.Errorf("%s: error = %v, want %v", name, err, outcome)
		}

		if runs != 1 {
			t.Errorf("%s: the work ran %d times, want 1", name, runs)
		}
	}
}
