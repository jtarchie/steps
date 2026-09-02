package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
func TestAdoptingAConfigSyncsConcurrency(t *testing.T) {
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
	target := NewPipeline("demo", path, cfg, st, events.New(nil))

	// Recovery and adoption are separate acts now — the daemon does both at
	// startup, and only the second on a reload. This test is about the
	// admission tables, which adoption owns.
	PrepareQueue(ctx, target)
	SyncQueueLimits(ctx, target)

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

	target := NewPipeline("demo", path, cfg, st, events.New(nil))

	PrepareQueue(ctx, target)

	for _, job := range []string{"first", "second"} {
		err = st.EnqueueJob(ctx, job, "test")
		if err != nil {
			t.Fatalf("EnqueueJob %s: %v", job, err)
		}
	}

	runner := NewLocalRunner(map[string]workspace.Provider{"demo": provider}, nil, 2, false)

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

// TestDrainHonorsTheCircuitBreaker.
//
// `max_consecutive_failures:` is documented against `steps web` (docs/infra.md),
// and `steps web` is the only long-running mode there is. Until this test
// existed the breaker was implemented only in internal/trigger's drainer,
// which the daemon does not use — so the count never advanced, the job never
// paused, and a broken job kept firing on every new version forever, which is
// the exact thing the feature exists to stop.
//
// Two halves, because the breaker has two: the count has to ADVANCE on a
// failure and trip at the ceiling, and a paused job has to be SKIPPED rather
// than claimed and run.
func TestDrainHonorsTheCircuitBreaker(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "demo.yml")

	writeFile(t, path, `
jobs:
  - name: build
    max_consecutive_failures: 2
    plan:
      - task: fail
        inputs: []
        run: "exit 1"
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

	ctx := t.Context()
	target := NewPipeline("demo", path, cfg, st, events.New(nil))

	PrepareQueue(ctx, target)

	runner := NewLocalRunner(map[string]workspace.Provider{"demo": provider}, nil, 1, false)

	// Two failures is the ceiling; the third row proves a paused job is not
	// run again.
	for range 3 {
		err = st.EnqueueJob(ctx, "build", "test")
		if err != nil {
			t.Fatalf("EnqueueJob: %v", err)
		}

		if !runner.drainOne(ctx, target) {
			t.Fatal("nothing was claimed from a queue with a pending row")
		}
	}

	paused, err := st.PausedJobs(ctx)
	if err != nil {
		t.Fatalf("PausedJobs: %v", err)
	}

	if len(paused) != 1 || paused[0].Name != "build" {
		t.Fatalf("paused jobs = %+v, want build paused after 2 consecutive failures", paused)
	}

	assertOneSkippedRow(ctx, t, st)
}

// assertOneSkippedRow is the second half of the breaker: a paused job is not
// claimed and run, it is finalized as skipped so the queue does not fill with
// work nobody intends to do.
func assertOneSkippedRow(ctx context.Context, t *testing.T, st *store.Store) {
	t.Helper()

	rows, err := st.ListTriggerQueue(ctx, 10)
	if err != nil {
		t.Fatalf("ListTriggerQueue: %v", err)
	}

	skipped := 0

	for _, row := range rows {
		if row.Status == "skipped" {
			skipped++
		}
	}

	if skipped != 1 {
		t.Errorf("skipped rows = %d, want 1: the drainer ran a job the breaker had paused (%+v)", skipped, rows)
	}
}

// TestDrainLeavesAnInterruptedRunRunning.
//
// store.CompleteJob's doc comment is explicit that a run stopped by ctx
// cancellation must NOT be finalized: the row stays `running` so the next
// startup's ResetStaleRunning re-queues it, since only a new version change
// would otherwise ever enqueue it again. The web drainer finalized it as
// `failed`, which both loses the re-run and — now that the breaker is here —
// would count an operator's ctrl-C against a job that was working.
func TestDrainLeavesAnInterruptedRunRunning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "demo.yml")

	writeFile(t, path, `
jobs:
  - name: build
    max_consecutive_failures: 1
    plan:
      - task: slow
        inputs: []
        timeout: 60s
        run: sleep 30
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

	target := NewPipeline("demo", path, cfg, st, events.New(nil))

	PrepareQueue(t.Context(), target)

	err = st.EnqueueJob(t.Context(), "build", "test")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	// Cancelled while the step sleeps, which is what a SIGTERM to the daemon
	// looks like from inside drainOne.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := NewLocalRunner(map[string]workspace.Provider{"demo": provider}, nil, 1, false)
	runner.drainOne(ctx, target)

	rows, err := st.ListTriggerQueue(t.Context(), 10)
	if err != nil {
		t.Fatalf("ListTriggerQueue: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("queue rows = %d, want 1", len(rows))
	}

	if rows[0].Status != "running" {
		t.Errorf("queue row status = %q, want running: an interrupted build was finalized, so nothing re-queues it", rows[0].Status)
	}

	paused, err := st.PausedJobs(t.Context())
	if err != nil {
		t.Fatalf("PausedJobs: %v", err)
	}

	if len(paused) != 0 {
		t.Errorf("paused = %+v, want none: ctrl-C is an operator, not a broken job", paused)
	}
}

// TestDrainAppliesProcessWideForce.
//
// `steps web --force` is the daemon form of a flag that already worked under
// `--once`, and it was declared on WebCmd while reaching nothing: the runner
// took its force only from the browser's "Re-run (forced)" button, so an
// operator who restarted the daemon with --force to escape a bad cache got
// every step skipped as cached and a green build that executed nothing.
//
// The proof is a side effect the cache cannot fake: a step that appends a
// line. Cached, the file stays one line; forced, it grows.
func TestDrainAppliesProcessWideForce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "demo.yml")
	tally := filepath.Join(dir, "ran.txt")

	writeFile(t, path, `
jobs:
  - name: build
    plan:
      - task: append
        inputs: []
        run: echo ran >> `+tally+`
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

	ctx := t.Context()
	target := NewPipeline("demo", path, cfg, st, events.New(nil))

	PrepareQueue(ctx, target)

	drain := func(t *testing.T, runner *LocalRunner) {
		t.Helper()

		enqueueErr := st.EnqueueJob(ctx, "build", "test")
		if enqueueErr != nil {
			t.Fatalf("EnqueueJob: %v", enqueueErr)
		}

		if !runner.drainOne(ctx, target) {
			t.Fatal("nothing was claimed from a queue with a pending row")
		}
	}

	plain := NewLocalRunner(map[string]workspace.Provider{"demo": provider}, nil, 1, false)
	drain(t, plain)

	// Second time through with no force: the content has not changed, so the
	// step is skipped and the tally must not grow. This is the control — it
	// is what makes the third run's growth mean force, not repetition.
	drain(t, plain)

	if lines := countLines(t, tally); lines != 1 {
		t.Fatalf("tally = %d lines after two unforced runs, want 1 (the cache is not doing its job, so this test cannot prove anything)", lines)
	}

	drain(t, NewLocalRunner(map[string]workspace.Provider{"demo": provider}, nil, 1, true))

	if lines := countLines(t, tally); lines != 2 {
		t.Errorf("tally = %d lines, want 2: --force did not reach the drainer, so the step ran cached", lines)
	}
}

// countLines is the tally a forced re-run grows and a cached one does not.
func countLines(t *testing.T, path string) int {
	t.Helper()

	//nolint:gosec // the path is this test's own t.TempDir()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tally: %v", err)
	}

	return len(strings.Fields(string(body)))
}
