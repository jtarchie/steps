package scheduler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/workspace"
)

// The cancel is the SIGINT path; what separates the two is the queue row and the breaker.
func TestAbortFinalizesTheRunItStopped(t *testing.T) {
	t.Parallel()

	runner, target, st := slowPipeline(t)
	done := make(chan struct{})

	go func() {
		defer close(done)

		runner.DrainOne(context.Background(), target)
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

	target := NewPipeline("demo", cfg, st, events.New(nil))

	PrepareQueue(t.Context(), target)

	err = st.EnqueueJob(t.Context(), "build", "test")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	return NewLocalRunner(map[string]workspace.Provider{"demo": provider}, pipeline.Run{}, 1), target, st
}

func assertStoppedCleanly(t *testing.T, st store.Store) {
	t.Helper()

	rows, err := st.ListTriggerQueue(t.Context(), 10)
	if err != nil || len(rows) != 1 || rows[0].Status != "aborted" {
		t.Errorf("queue = %+v (%v), want its one row aborted — left running, a restart re-runs what somebody stopped", rows, err)
	}

	paused, err := st.HeldJobs(t.Context())
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
