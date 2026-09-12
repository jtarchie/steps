package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
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

	st, err := sqlite.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
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

	st, err := sqlite.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
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
func succeededQueueRows(ctx context.Context, t *testing.T, st store.Store) int {
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

	st, err := sqlite.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
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

// TestATrippedBreakerSaysSo: the pause itself is a row nobody is looking at, so the daemon's log is the only thing that tells anybody the job stopped. Serial: the logger is the process's.
func TestATrippedBreakerSaysSo(t *testing.T) {
	logs := captureLogs(t)

	runner, target, _ := drainable(t, t.TempDir(), `
jobs:
  - name: build
    max_consecutive_failures: 1
    plan:
      - task: fail
        inputs: []
        run: "exit 1"
`)

	runner.drainOne(t.Context(), target)

	if !strings.Contains(logs.String(), "web.job_paused") {
		t.Errorf("the breaker tripped without a word:\n%s", logs.String())
	}

	// An error logged for a store call that worked trains an operator to skim past the one that did not.
	for _, key := range []string{"web.reset_stale", "web.sync_job_limits", "web.complete"} {
		if strings.Contains(logs.String(), key) {
			t.Errorf("a clean drain logged %s:\n%s", key, logs.String())
		}
	}
}

// A pass must clear the count: counted as a failure, or merely left standing, fail-pass-fail trips a limit of two. Forced, or the pass is a chain-skip hit for the last run.
func TestAPassingRunLeavesTheBreakerAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pass := filepath.Join(dir, "pass")

	runner, target, st := drainable(t, dir, `
jobs:
  - name: build
    max_consecutive_failures: 2
    plan:
      - task: flip
        inputs: []
        run: test -f `+pass+`
`)
	ctx := t.Context()

	runner.drainOne(ctx, target)

	writeFile(t, pass, "")

	for _, passing := range []bool{true, false} {
		if !passing {
			err := os.Remove(pass)
			if err != nil {
				t.Fatalf("remove: %v", err)
			}
		}

		_, err := runner.Enqueue(ctx, target, "build", "test", true)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}

		if !runner.drainOne(ctx, target) {
			t.Fatal("nothing was claimed from a queue with a pending row")
		}
	}

	if got := queueStatuses(t, st); got != "failed succeeded failed" {
		t.Fatalf("queue = %q, want failed succeeded failed", got)
	}

	paused, err := st.PausedJobs(ctx)
	if err != nil {
		t.Fatalf("PausedJobs: %v", err)
	}

	if len(paused) != 0 {
		t.Errorf("paused jobs = %+v: a passing run between two failures still counted toward max_consecutive_failures: 2", paused)
	}
}

// assertOneSkippedRow is the second half of the breaker: a paused job is not
// claimed and run, it is finalized as skipped so the queue does not fill with
// work nobody intends to do.
func assertOneSkippedRow(ctx context.Context, t *testing.T, st store.Store) {
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

	st, err := sqlite.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	time.AfterFunc(2*time.Second, cancel) // cancelled, never timed out: DeadlineExceeded is what a step's own timeout: ends in, which is the job answering

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

	st, err := sqlite.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
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

// A step's own timeout: is the job answering, not the daemon going down: read as an interruption the row stayed running, so a serial job admitted nothing after it until a restart, the breaker never counted it, and the restart re-ran a build that had already failed.
func TestDrainFinalizesAStepTimeoutAsAFailure(t *testing.T) {
	t.Parallel()

	runner, target, st := drainable(t, t.TempDir(), `
jobs:
  - name: build
    serial: true
    max_consecutive_failures: 1
    plan:
      - task: slow
        inputs: []
        timeout: 200ms
        run: sleep 10
`)
	ctx := t.Context()

	runner.drainOne(ctx, target)

	err := st.EnqueueJob(ctx, "build", "next")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	if !runner.drainOne(ctx, target) {
		t.Fatal("the next build of a serial job could not be claimed: the timed-out build still holds its slot")
	}

	if got := queueStatuses(t, st); got != "failed skipped" {
		t.Errorf("queue = %q, want the timed-out build failed and the next one skipped by the breaker it tripped", got)
	}

	paused, err := st.PausedJobs(ctx)
	if err != nil || len(paused) != 1 {
		t.Errorf("paused = %+v (%v), want build: a timeout is a failure the breaker counts", paused, err)
	}
}

// The drain decides, not the error chain alone: a cancellation with the drain still live is the job's own answer, and a timeout: expiring while a shutdown waits is still the job failing.
func TestInterruptedAsksTheDrain(t *testing.T) {
	t.Parallel()

	if interrupted(t.Context(), fmt.Errorf("step: %w", context.Canceled)) {
		t.Error("a cancellation with the drain still live read as this process cutting the run short")
	}

	ended, end := context.WithCancel(t.Context())
	end()

	if !interrupted(ended, fmt.Errorf("step: %w", context.Canceled)) {
		t.Error("a run the drain's end cancelled did not read as interrupted")
	}

	if interrupted(ended, fmt.Errorf("step: %w", context.DeadlineExceeded)) {
		t.Error("a step's own timeout: read as an interruption because a shutdown happened to be waiting on it")
	}
}

// TestConformanceNonInterruptibleBuildSurvivesShutdown: concourse-ci.org/docs/jobs/ — interruptible defaults to false, and only a job that says true is not waited for at shutdown, since a deploy half-applied by a restart is the case the field exists for.
func TestConformanceNonInterruptibleBuildSurvivesShutdown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	finished := filepath.Join(dir, "finished")

	runner, target, st := drainable(t, dir, `
jobs:
  - name: build
    plan:
      - task: deploy
        inputs: []
        run: |
          touch `+started+`
          sleep 1
          touch `+finished+`
`)

	process, shutdown := context.WithCancel(t.Context())
	defer shutdown()

	runner.StopWith(process)

	drain, stop := context.WithCancel(process)
	defer stop()

	done := drainInBackground(drain, runner, target)

	waitForFile(t, started)
	shutdown()
	waitForDrain(t, done, 20*time.Second)

	if got := queueStatuses(t, st); got != "succeeded" {
		t.Errorf("queue = %q, want the build finished and recorded: cut off, the next start re-runs a deploy", got)
	}

	_, err := os.Stat(finished)
	if err != nil {
		t.Errorf("the build was cut off by the shutdown: %v", err)
	}
}

// What does not wait: a job that said interruptible: true, a drain stopped with the process alive (its pipeline destroyed or renamed, which must not wait out the grace), and a build outlasting the grace — each left running for the next start to re-queue.
func TestDrainCancelsWhatDoesNotWait(t *testing.T) {
	t.Parallel()

	for name, scenario := range map[string]struct {
		interruptible, shutdown bool
		grace                   time.Duration
	}{
		"interruptible at shutdown":     {interruptible: true, shutdown: true},
		"pipeline destroyed mid-build":  {},
		"shutdown outlasting the grace": {shutdown: true, grace: 100 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			started := filepath.Join(dir, "started")

			runner, target, st := drainable(t, dir, fmt.Sprintf(`
jobs:
  - name: build
    interruptible: %t
    plan:
      - task: deploy
        inputs: []
        run: |
          touch %s
          sleep 30
`, scenario.interruptible, started))

			if scenario.grace > 0 {
				runner.grace = scenario.grace
			}

			process, shutdown := context.WithCancel(t.Context())
			defer shutdown()

			runner.StopWith(process)

			drain, stop := context.WithCancel(process)
			defer stop()

			done := drainInBackground(drain, runner, target)

			waitForFile(t, started)

			if scenario.shutdown {
				shutdown()
			} else {
				stop()
			}

			waitForDrain(t, done, 10*time.Second)

			if got := queueStatuses(t, st); got != "running" {
				t.Errorf("queue = %q, want running: cut short by this process, the build is the next start's to re-queue", got)
			}
		})
	}
}

// A row the breaker skips still spent the force it was claimed with: left behind, the flag forces the job's next ordinary build, re-running from scratch what the cache would have skipped.
func TestABreakerSkipSpendsTheForce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tally := filepath.Join(dir, "ran.txt")

	runner, target, st := drainable(t, dir, `
jobs:
  - name: build
    plan:
      - task: append
        inputs: []
        run: echo ran >> `+tally+`
`)
	ctx := t.Context()

	runner.drainOne(ctx, target)

	_, _, err := st.RecordJobOutcome(ctx, "build", false, 1)
	if err != nil {
		t.Fatalf("RecordJobOutcome: %v", err)
	}

	_, err = runner.Enqueue(ctx, target, "build", "re-run", true)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	runner.drainOne(ctx, target)

	_, _, err = st.RecordJobOutcome(ctx, "build", true, 1)
	if err != nil {
		t.Fatalf("RecordJobOutcome: %v", err)
	}

	err = st.EnqueueJob(ctx, "build", "poll")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	runner.drainOne(ctx, target)

	if got := queueStatuses(t, st); got != "succeeded skipped succeeded" {
		t.Fatalf("queue = %q, want the forced row skipped by the breaker between two ordinary builds", got)
	}

	if lines := countLines(t, tally); lines != 1 {
		t.Errorf("tally = %d, want 1: the skipped row's force re-ran the next ordinary build from scratch", lines)
	}
}

// drainable is one pipeline with one queued build of "build", for a test that drives drainOne itself.
func drainable(t *testing.T, dir, yaml string) (*LocalRunner, *Pipeline, store.Store) {
	t.Helper()

	path := filepath.Join(dir, "demo.yml")
	writeFile(t, path, yaml)

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := sqlite.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
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
	SyncQueueLimits(t.Context(), target)

	err = st.EnqueueJob(t.Context(), "build", "test")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	return NewLocalRunner(map[string]workspace.Provider{"demo": provider}, nil, 1, false), target, st
}

// panicsOnce panics on its first build, the way a bug deep inside RunJob reaches the drain.
type panicsOnce struct {
	workspace.Provider

	fired bool
}

func (p *panicsOnce) NewBuild(ctx context.Context, label string) (workspace.BuildWorkspace, error) {
	if !p.fired {
		p.fired = true

		panic("workspace exploded")
	}

	return p.Provider.NewBuild(ctx, label) //nolint:wrapcheck // the wrapped provider's own answer
}

// A panic inside one run took the daemon down with every pipeline it holds; it is one failed run, and the serial slot it held is given back.
func TestDrainSurvivesAPanickingRun(t *testing.T) {
	t.Parallel()

	runner, target, st := drainable(t, t.TempDir(), `
jobs:
  - name: build
    serial: true
    plan:
      - task: work
        inputs: []
        run: "true"
`)

	provider, err := workspace.NewProvider(target.Config().Workspace, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	runner.SetProvider("demo", &panicsOnce{Provider: provider})

	if !runner.drainOne(t.Context(), target) {
		t.Fatal("nothing was claimed from a queue with a pending row")
	}

	rows, err := st.ListTriggerQueue(t.Context(), 10)
	if err != nil {
		t.Fatalf("ListTriggerQueue: %v", err)
	}

	if len(rows) != 1 || rows[0].Status != "failed" || !strings.Contains(rows[0].Error, "workspace exploded") {
		t.Fatalf("queue = %+v, want the panicking run failed with the panic named", rows)
	}

	err = st.EnqueueJob(t.Context(), "build", "test")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	if !runner.drainOne(t.Context(), target) {
		t.Fatal("the serial slot the panicking run held was never given back")
	}

	if got := queueStatuses(t, st); got != "failed succeeded" {
		t.Errorf("queue = %q, want the next build to run after the panic", got)
	}
}

// queueStatuses is the queue oldest first.
func queueStatuses(t *testing.T, st store.Store) string {
	t.Helper()

	rows, err := st.ListTriggerQueue(t.Context(), 10)
	if err != nil {
		t.Fatalf("ListTriggerQueue: %v", err)
	}

	statuses := make([]string, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		statuses = append(statuses, rows[i].Status)
	}

	return strings.Join(statuses, " ")
}

func drainInBackground(drain context.Context, runner *LocalRunner, target *Pipeline) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		defer close(done)

		runner.drainOne(drain, target)
	}()

	return done
}

func waitForDrain(t *testing.T, done <-chan struct{}, within time.Duration) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("drainOne was still running %s later", within)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)

	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		if err == nil {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("%s never appeared", path)
}
