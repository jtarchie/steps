package web

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
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
		err = pipeline.Store.StartRun(ctx, store.NewRun{ID: id, JobName: "build", Workspace: "/tmp/ws"})
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

	assertAbortRefusalsSayWhy(t, server)

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

// Both refusals are 409, so only the message tells the operator whether to go find the other process or stop trying.
func assertAbortRefusalsSayWhy(t *testing.T, server *Server) {
	t.Helper()

	for id, want := range map[string]string{
		"orphaned": "not running on this daemon",
		"finished": "already passed",
	} {
		_, body := call(t, server, http.MethodPost, "/api/pipelines/demo/runs/"+id+"/abort", "")
		if !strings.Contains(body, want) {
			t.Errorf("aborting %s answered %s, want it to say %q", id, body, want)
		}
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

	runner.DrainOne(t.Context(), target.Pipeline)

	if status := post(t, server, "/p/demo/jobs/build/trigger", map[string]string{"force": "all"}); status != http.StatusSeeOther {
		t.Fatalf("forced trigger = %d, want 303", status)
	}

	if status := post(t, server, "/api/pipelines/demo/jobs/build/queued/abort", nil); status != http.StatusNoContent {
		t.Fatalf("queued abort = %d, want 204", status)
	}

	err = st.EnqueueJob(t.Context(), "build", "poll")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	runner.DrainOne(t.Context(), target.Pipeline)

	if lines := countLines(t, tally); lines != 1 {
		t.Errorf("tally = %d, want 1: the aborted row's force re-ran the next ordinary build from scratch", lines)
	}
}
