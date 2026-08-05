package trigger

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// TestBreakerPausesAfterConsecutiveFailures covers the whole point: a job that
// fails on every new version keeps firing on every new version, burning model
// spend or CI minutes on a failure no automatic retry will fix — unattended,
// over a weekend, with nobody told.
func TestBreakerPausesAfterConsecutiveFailures(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, t.TempDir())
	ctx := context.Background()

	for attempt := 1; attempt <= 2; attempt++ {
		paused, consecutive, err := st.RecordJobOutcome(ctx, "nightly", false, 3)
		if err != nil {
			t.Fatalf("RecordJobOutcome: %v", err)
		}

		if paused {
			t.Fatalf("paused after %d of 3 failures", attempt)
		}

		if consecutive != attempt {
			t.Errorf("consecutive = %d, want %d", consecutive, attempt)
		}
	}

	paused, consecutive, err := st.RecordJobOutcome(ctx, "nightly", false, 3)
	if err != nil {
		t.Fatalf("RecordJobOutcome: %v", err)
	}

	if !paused || consecutive != 3 {
		t.Fatalf("paused=%v consecutive=%d, want true and 3", paused, consecutive)
	}

	isPaused, err := st.IsJobPaused(ctx, "nightly")
	if err != nil || !isPaused {
		t.Fatalf("IsJobPaused = %v, %v; want true", isPaused, err)
	}
}

// TestBreakerResetsOnSuccess verifies the counter measures CONSECUTIVE
// failures. A job that fails, passes, then fails again is flaky, not broken,
// and tripping on it would be the opposite of the intent.
func TestBreakerResetsOnSuccess(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, t.TempDir())
	ctx := context.Background()

	for _, succeeded := range []bool{false, false, true, false} {
		_, _, err := st.RecordJobOutcome(ctx, "flaky", succeeded, 3)
		if err != nil {
			t.Fatalf("RecordJobOutcome: %v", err)
		}
	}

	paused, consecutive, err := st.RecordJobOutcome(ctx, "flaky", false, 3)
	if err != nil {
		t.Fatalf("RecordJobOutcome: %v", err)
	}

	if paused {
		t.Error("a flaky job was paused; the success between failures must reset the count")
	}

	if consecutive != 2 {
		t.Errorf("consecutive = %d, want 2 (counting only since the last success)", consecutive)
	}
}

// TestBreakerOffByDefault keeps the feature opt-in: a job that declares no
// max_consecutive_failures never pauses, however many times it fails.
func TestBreakerOffByDefault(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, t.TempDir())
	ctx := context.Background()

	for range 10 {
		paused, _, err := st.RecordJobOutcome(ctx, "unguarded", false, 0)
		if err != nil {
			t.Fatalf("RecordJobOutcome: %v", err)
		}

		if paused {
			t.Fatal("a job with no breaker configured was paused")
		}
	}

	// The count is still kept, so turning a breaker on later starts from a
	// real number rather than pretending the history did not happen.
	paused, consecutive, err := st.RecordJobOutcome(ctx, "unguarded", false, 3)
	if err != nil {
		t.Fatalf("RecordJobOutcome: %v", err)
	}

	if !paused || consecutive != 11 {
		t.Errorf("paused=%v consecutive=%d, want true and 11", paused, consecutive)
	}
}

// TestResumeClearsTheBreaker covers the manual half. Resume is deliberately
// manual in v1: unattended auto-resume defeats the safety purpose when the
// underlying breakage — a dead API key, say — has not actually been fixed.
func TestResumeClearsTheBreaker(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, t.TempDir())
	ctx := context.Background()

	_, _, err := st.RecordJobOutcome(ctx, "nightly", false, 1)
	if err != nil {
		t.Fatalf("RecordJobOutcome: %v", err)
	}

	paused, err := st.PausedJobs(ctx)
	if err != nil || len(paused) != 1 || paused[0].Name != "nightly" {
		t.Fatalf("PausedJobs = %v, %v; want one entry for nightly", paused, err)
	}

	err = st.ResetJobFailures(ctx, "nightly")
	if err != nil {
		t.Fatalf("ResetJobFailures: %v", err)
	}

	isPaused, err := st.IsJobPaused(ctx, "nightly")
	if err != nil || isPaused {
		t.Fatalf("IsJobPaused = %v, %v; want false after a resume", isPaused, err)
	}
}

// TestDrainOneSkipsAPausedJob verifies a paused job is taken out of the
// rotation rather than merely reported: the queue must not fill with work
// nobody intends to do.
func TestDrainOneSkipsAPausedJob(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	st := mustOpenStore(t, dir)
	ctx := context.Background()

	cfg := &config.Config{Jobs: []config.Job{{
		Name:                   "nightly",
		MaxConsecutiveFailures: 1,
		Plan: []config.Step{{
			Task: "work", Run: "exit 1", Inputs: config.Inputs(),
		}},
	}}}

	err := st.ResetJobFailures(ctx, "nightly")
	if err != nil {
		t.Fatalf("ResetJobFailures: %v", err)
	}

	_, _, err = st.RecordJobOutcome(ctx, "nightly", false, 1)
	if err != nil {
		t.Fatalf("RecordJobOutcome: %v", err)
	}

	err = st.EnqueueJob(ctx, "nightly", "a new version")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	defer func() { _ = provider.Close() }()

	output := captureStdout(t, func() {
		_, err = drainOne(ctx, cfg, provider, st, nil, false)
	})
	if err != nil {
		t.Fatalf("drainOne: %v", err)
	}

	if !strings.Contains(output, "is paused") {
		t.Errorf("drainOne did not say the job was paused:\n%s", output)
	}

	// Nothing pending is left behind.
	if queued := pendingCount(t, st); queued != 0 {
		t.Errorf("pending rows = %d, want 0 — a skipped row must be finalized", queued)
	}
}

// pendingCount reports how many queue rows are still waiting, by trying to
// claim one: a claimable row means work is still queued.
func pendingCount(t *testing.T, st *store.Store) int {
	t.Helper()

	_, _, found, err := st.ClaimNextJob(context.Background())
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}

	if !found {
		return 0
	}

	return 1
}
