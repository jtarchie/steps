package web

import (
	"context"
	"net/http"
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

// --read-only says nothing from outside may stop work here, and a terminal reaching the API is outside too.
func TestAbortIsWithheldFromAReadOnlyServer(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	for _, target := range []string{
		"/p/demo/runs/some-run/abort",
		"/p/demo/jobs/build/queued/abort",
		"/api/pipelines/demo/runs/some-run/abort",
		"/api/pipelines/demo/jobs/build/queued/abort",
	} {
		status := post(t, server, target, nil)
		if status != http.StatusForbidden {
			t.Errorf("POST %s = %d, want 403", target, status)
		}
	}

	_, follow := get(t, server, "/p/demo/jobs/build/follow")
	if strings.Contains(follow, "queued/abort") {
		t.Error("a read-only server's waiting room offers to abort")
	}
}

// A run nothing here is executing has no context to cancel, and answering as if it had been stopped would be the page lying about it.
func TestAbortRefusesARunThisDaemonIsNotExecuting(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)
	ctx := context.Background()

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, id := range []string{"finished", "orphaned"} {
		err = pipeline.Store.StartRun(ctx, id, "build", "/tmp/ws", "")
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
	}

	err = pipeline.Store.FinishRun(ctx, "finished", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	for target, want := range map[string]int{
		"/api/pipelines/demo/runs/finished/abort":     http.StatusConflict,
		"/api/pipelines/demo/runs/orphaned/abort":     http.StatusConflict,
		"/api/pipelines/demo/runs/nobody/abort":       http.StatusNotFound,
		"/api/pipelines/nowhere/runs/finished/abort":  http.StatusNotFound,
		"/api/pipelines/demo/jobs/build/queued/abort": http.StatusConflict,
		"/p/demo/runs/finished/abort":                 http.StatusConflict,
		"/p/demo/jobs/build/queued/abort":             http.StatusConflict,
	} {
		status := post(t, server, target, nil)
		if status != want {
			t.Errorf("POST %s = %d, want %d", target, status, want)
		}
	}

	_, running := get(t, server, "/p/demo/runs/orphaned")
	if !strings.Contains(running, `action="/p/demo/runs/orphaned/abort"`) {
		t.Error("a running run's page offers no abort")
	}

	_, finished := get(t, server, "/p/demo/runs/finished")
	if strings.Contains(finished, "/abort") {
		t.Error("a finished run's page offers to abort it")
	}

	// The waiting room is the queued build's page, and where a person lands after a trigger they did not mean.
	_, follow := get(t, server, "/p/demo/jobs/build/follow")
	if !strings.Contains(follow, `action="/p/demo/jobs/build/queued/abort"`) {
		t.Error("the page a trigger lands on offers no way to take it back")
	}
}

// The cancel is the SIGINT path; what separates the two is the queue row and the breaker.
func TestAbortFinalizesTheRunItStopped(t *testing.T) {
	t.Parallel()

	runner, target, st := slowPipeline(t)
	done := make(chan struct{})

	go func() {
		defer close(done)

		runner.drainOne(context.Background(), target)
	}()

	runID := runningRunID(t, st)

	if runner.Abort(&Pipeline{Slug: "other"}, runID) {
		t.Error("a run was stopped through a pipeline it does not belong to")
	}

	if !runner.Abort(target, runID) {
		t.Fatal("Abort could not find the run this runner is executing")
	}

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the aborted run never returned")
	}

	assertStoppedCleanly(t, st)

	if runner.Abort(target, runID) {
		t.Error("an ended run could still be aborted: its cancel outlived it")
	}
}

// The force flag is keyed by job, since a queue row has no column for it, so dropping the forced row must drop the flag — or the job's next ordinary build ignores the cache, re-billing an agent for work nobody asked to redo.
func TestAbortingAForcedQueuedRunDropsItsForce(t *testing.T) {
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

	server, err := New([]*Pipeline{target}, runner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runner.drainOne(t.Context(), target)

	if status := post(t, server, "/p/demo/jobs/build/trigger", map[string]string{"force": "1"}); status != http.StatusSeeOther {
		t.Fatalf("forced trigger = %d, want 303", status)
	}

	if status := post(t, server, "/api/pipelines/demo/jobs/build/queued/abort", nil); status != http.StatusNoContent {
		t.Fatalf("queued abort = %d, want 204", status)
	}

	err = st.EnqueueJob(t.Context(), "build", "poll")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	runner.drainOne(t.Context(), target)

	if lines := countLines(t, tally); lines != 1 {
		t.Errorf("tally = %d, want 1: the aborted row's force re-ran the next ordinary build from scratch", lines)
	}
}

// slowPipeline queues a job that runs until something stops it, with a breaker set to trip on the first failure it counts.
func slowPipeline(t *testing.T) (*LocalRunner, *Pipeline, store.Store) {
	t.Helper()

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

	return NewLocalRunner(map[string]workspace.Provider{"demo": provider}, nil, 1, false), target, st
}

func assertStoppedCleanly(t *testing.T, st store.Store) {
	t.Helper()

	rows, err := st.ListTriggerQueue(t.Context(), 10)
	if err != nil || len(rows) != 1 || rows[0].Status != "aborted" {
		t.Errorf("queue = %+v (%v), want its one row aborted — left running, a restart re-runs what somebody stopped", rows, err)
	}

	paused, err := st.PausedJobs(t.Context())
	if err != nil || len(paused) != 0 {
		t.Errorf("paused = %+v (%v), want none: stopping a build is an operator's decision, not the job breaking", paused, err)
	}
}

func runningRunID(t *testing.T, st store.Runs) string {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)

	for time.Now().Before(deadline) {
		runs, err := st.ListRuns(t.Context(), "build", 1)
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}

		if len(runs) == 1 {
			return runs[0].ID
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("the job never recorded a run")

	return ""
}
