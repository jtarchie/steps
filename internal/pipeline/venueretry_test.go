package pipeline

// The re-placement loop, which had no tests at all: the vacuity audit proved
// that deleting the budget refusal, the ctx check, or the retry.Stop wrap
// shipped green through the entire suite.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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

		// Bounded, so a cap that never trips fails here rather than spinning until the suite's timeout.
		if runs > venueRetries+1 {
			return "", errors.New("re-placed past the cap")
		}

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

// A get or put dials its resource's tag for the check as well as its own, so both have to be mapped before anything runs.
func TestAResourceStepDialsItsResourcesTagToo(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Resources: []config.Resource{{Name: "repo", Tags: []string{"b"}}}}

	for name, c := range map[string]struct {
		step config.Step
		want []string
	}{
		"its own tag and the resource's": {config.Step{Get: "repo", Tags: []string{"a"}}, []string{"a", "b"}},
		"the resource's tag as its own":  {config.Step{Put: "repo", Tags: []string{"b"}}, []string{"b"}},
		"no tag of its own":              {config.Step{Get: "repo"}, []string{"b"}},
		"a task":                         {config.Step{Task: "work", Tags: []string{"a"}}, []string{"a"}},
	} {
		got := stepPlacementTags(cfg, c.step)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: dials %v, want %v", name, got, c.want)
		}
	}
}

func TestAPollRefusesOnlyATaggedResourceNobodyMapped(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Resources: []config.Resource{{Name: "plain"}, {Name: "remote", Tags: []string{"gpu"}}}}

	err := ValidatePipelinePlacement(context.Background(), cfg, []string{"plain"})
	if err != nil {
		t.Errorf("an untagged resource was refused: %v", err)
	}

	err = ValidatePipelinePlacement(context.Background(), cfg, []string{"remote"})
	if err == nil || !strings.Contains(err.Error(), "tag gpu") {
		t.Errorf("a resource tagged for an unmapped worker: err = %v, want it refused naming tag gpu", err)
	}
}

// ?binary= reaches an aws:// worker only through the artifact store, so whether one is set decides the refusal, for a job and for a poll alike.
func TestPlacementChecksKnowWhetherAnArtifactStoreIsSet(t *testing.T) {
	t.Parallel()

	ctx, err := WithWorkers(context.Background(), map[string]string{"gpu": "aws://i-0abc123def456789?binary=/tmp/steps-linux-amd64"})
	if err != nil {
		t.Fatalf("WithWorkers: %v", err)
	}

	cfg := &config.Config{Resources: []config.Resource{{Name: "remote", Tags: []string{"gpu"}}}}
	job := &config.Job{Name: "build", Plan: []config.Step{{Task: "work", Tags: []string{"gpu"}}}}

	for _, withStore := range []bool{false, true} {
		checked := ctx
		if withStore {
			checked = WithArtifactStore(ctx, "s3://bucket/steps")
		}

		for check, err := range map[string]error{
			"poll": ValidatePipelinePlacement(checked, cfg, []string{"remote"}),
			"job":  ValidateWorkerPlacement(checked, cfg, job),
		} {
			if (err == nil) != withStore || (err != nil && !errors.Is(err, venue.ErrWorker)) {
				t.Errorf("artifact store set=%v, %s: err = %v, want refused=%v by the placement check", withStore, check, err, !withStore)
			}
		}
	}
}
