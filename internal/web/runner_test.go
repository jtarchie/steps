package web

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// TestPrepareQueueSyncsConcurrency covers the admission input `steps web`
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

	PrepareQueue(ctx, target)

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

// TestDrainRunsUpToMaxConcurrent.
//
// `steps watch --max-concurrent` bounded how many queued jobs a daemon ran at
// once; when watch folded into web, the drainer had to grow the same bound or
// the flag would have quietly become a no-op. One worker per pipeline is not
// a detail: the queue this drains is also where a browser trigger and an
// approval release land, so a single long build used to make all of them
// wait.
//
// The two jobs rendezvous through the filesystem — each waits for the other's
// file — so they can only both finish if they run at the same time. With one
// worker the first would wait for a file the second cannot write until the
// first returns, which the per-job timeout ends.
func TestDrainRunsUpToMaxConcurrent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "demo.yml")
	first := filepath.Join(dir, "first.flag")
	second := filepath.Join(dir, "second.flag")

	writeFile(t, path, `
jobs:
  - name: first
    plan:
      - task: rendezvous
        inputs: []
        timeout: 20s
        run: |
          touch `+first+`
          until [ -f `+second+` ]; do sleep 0.05; done
  - name: second
    plan:
      - task: rendezvous
        inputs: []
        timeout: 20s
        run: |
          touch `+second+`
          until [ -f `+first+` ]; do sleep 0.05; done
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

	provider, err := workspace.NewProvider(cfg.Workspace, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	target := &Pipeline{Slug: "demo", Path: path, Cfg: cfg, Store: st, Bus: events.New(nil)}

	PrepareQueue(ctx, target)

	for _, job := range []string{"first", "second"} {
		err = st.EnqueueJob(ctx, job, "test")
		if err != nil {
			t.Fatalf("EnqueueJob %s: %v", job, err)
		}
	}

	runner := NewLocalRunner(map[string]workspace.Provider{"demo": provider}, nil, 2)

	done := make(chan struct{})

	go func() {
		defer close(done)

		runner.Drain(ctx, []*Pipeline{target})
	}()

	defer func() {
		cancel()
		<-done
	}()

	// Both jobs SUCCEEDING is the proof they overlapped, and the reason the
	// assertion is not "both flags exist": run one at a time, the first job
	// blocks on a file the second cannot write until it returns, and the
	// step timeout ends it — leaving its flag on disk and the second job
	// free to finish. The flags would both be there; one of the jobs would
	// have failed to get them.
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if succeededQueueRows(ctx, t, st) == 2 {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatal("the two jobs never both finished: the drainer ran them one at a time")
}

// succeededQueueRows counts finished-and-green queue rows, failing the test on
// the first red one — which under one worker is how this test ends: the job
// that blocked waiting for the other is killed by its own step timeout.
func succeededQueueRows(ctx context.Context, t *testing.T, st *store.Store) int {
	t.Helper()

	rows, err := st.ListTriggerQueue(ctx, 10)
	if err != nil {
		t.Fatalf("ListTriggerQueue: %v", err)
	}

	succeeded := 0

	for _, row := range rows {
		if row.Status == "failed" {
			t.Fatalf("job %s failed (%s): the drainer ran them one at a time", row.JobName, row.Error)
		}

		if row.Status == "succeeded" {
			succeeded++
		}
	}

	return succeeded
}
