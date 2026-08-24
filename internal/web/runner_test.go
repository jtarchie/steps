package web

import (
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// TestPrepareQueueSyncsConcurrency covers the admission input `steps watch`
// syncs and the web runner forgot: without job_concurrency rows,
// ClaimNextJob's COALESCE defaults every job to one build at a time, so a
// web-only deployment silently ignored max_in_flight.
func TestPrepareQueueSyncsConcurrency(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "demo.yml")

	writeFile(t, path, `
jobs:
  - name: build
    max_in_flight: 2
    plan:
      - task: compile
        run: "true"
`)

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := store.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	ctx := t.Context()
	target := &Pipeline{Slug: "demo", Path: path, Cfg: cfg, Store: st, Bus: events.New(nil)}

	PrepareQueue(ctx, target, true)

	// Claim two builds of the job without completing either: the second claim
	// is the one a missing job_concurrency row denies.
	for attempt := 1; attempt <= 2; attempt++ {
		err = st.EnqueueJob(ctx, "build", "test")
		if err != nil {
			t.Fatalf("EnqueueJob %d: %v", attempt, err)
		}

		_, jobName, claimed, err := st.ClaimNextJob(ctx)
		if err != nil {
			t.Fatalf("ClaimNextJob %d: %v", attempt, err)
		}

		if !claimed {
			t.Fatalf("claim %d: nothing claimable — max_in_flight was not synced, so the job is pinned to one build", attempt)
		}

		if jobName != "build" {
			t.Fatalf("claim %d: claimed %q, want %q", attempt, jobName, "build")
		}
	}
}
