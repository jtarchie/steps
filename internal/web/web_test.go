package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// testPipeline writes a pipeline YAML, opens its store, and returns a server
// over it. The YAML declares a passed: constraint so the dependency graph has
// something real to derive.
func testPipeline(t *testing.T) (*Server, *Pipeline) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "demo.yml")

	writeFile(t, path, `
resources:
  - name: repo
    type: mock
    source: {}

jobs:
  - name: build
    plan:
      - get: repo
      - task: compile
        run: "true"
  - name: deploy
    plan:
      - get: repo
        passed: [build]
      - task: ship
        run: "true"
`)

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := store.OpenStore(filepath.Join(dir, ".steps", "state.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	pipeline := &Pipeline{Slug: "demo", Path: path, Cfg: cfg, Store: st, Bus: events.New(nil)}

	server, err := New([]*Pipeline{pipeline}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return server, pipeline
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()

	err := writeFileRaw(path, body)
	if err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// get performs a GET and returns the status and body.
func get(t *testing.T, server *Server, target string) (int, string) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Code, rec.Body.String()
}

// TestJobsBoardShowsDependencyEdges is the graph question the CLI could never
// answer: `deploy` must show that it waits on `build`, and `build` must show
// that it feeds `deploy` — both derived from one passed: constraint.
func TestJobsBoardShowsDependencyEdges(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	code, body := get(t, server, "/p/demo")
	if code != http.StatusOK {
		t.Fatalf("GET /p/demo = %d", code)
	}

	for _, want := range []string{"build", "deploy", "repo"} {
		if !strings.Contains(body, want) {
			t.Errorf("jobs board missing %q", want)
		}
	}

	_, deployBody := get(t, server, "/p/demo/jobs/deploy")
	if !strings.Contains(deployBody, "must be green for") {
		t.Error("deploy page does not show its upstream constraint")
	}

	_, buildBody := get(t, server, "/p/demo/jobs/build")
	if !strings.Contains(buildBody, "waits on") {
		t.Error("build page does not show what it feeds")
	}
}

// TestRunTranscriptRendersStepsAndAgentTurns walks the whole event pipeline:
// events persisted exactly as a run would publish them, then read back as a
// transcript with a cached step folded and an agent's turns beneath it.
func TestRunTranscriptRendersStepsAndAgentTurns(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-1", "build", "/tmp/ws")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-1", []store.RunEventRow{
		{Type: events.TypeJobStarted, JobName: "build", StepIndex: -1},
		{Type: events.TypeStepStarted, JobName: "build", StepIndex: 0, StepName: "repo", StepKind: "get"},
		{Type: events.TypeStepSkipped, JobName: "build", StepIndex: 0, StepName: "repo", StepKind: "get", Status: "skipped", Hash: "cafe1234567", Text: "unchanged — replayed from cache"},
		{Type: events.TypeStepStarted, JobName: "build", StepIndex: 1, StepName: "review", StepKind: "agent"},
		{Type: events.TypeAgentText, JobName: "build", StepIndex: 1, StepName: "review", Text: "Reading the diff first."},
		{Type: events.TypeAgentCall, JobName: "build", StepIndex: 1, StepName: "review", Name: "read_file", Detail: `{"path":"main.go"}`},
		{Type: events.TypeAgentSubagent, JobName: "build", StepIndex: 1, StepName: "review", Name: "test-runner", Text: "run the suite", Status: "depth:1"},
		{Type: events.TypeStepFinished, JobName: "build", StepIndex: 1, StepName: "review", StepKind: "agent", Status: "succeeded", Hash: "beef7654321", DurationMS: 4200},
	})

	err = pipeline.Store.FinishRun(ctx, "run-1", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	code, body := get(t, server, "/p/demo/runs/run-1")
	if code != http.StatusOK {
		t.Fatalf("GET run = %d: %s", code, body)
	}

	// The cached step renders folded, carrying its reason — the product's
	// central mechanism made visible.
	if !strings.Contains(body, "unchanged — replayed from cache") {
		t.Error("transcript does not show why the cached step was skipped")
	}

	if !strings.Contains(body, `class="step skipped`) {
		t.Error("cached step is not rendered as skipped")
	}

	// The agent's turns hang under its step, including the delegation.
	for _, want := range []string{"Reading the diff first.", "read_file", "test-runner"} {
		if !strings.Contains(body, want) {
			t.Errorf("transcript missing agent turn %q", want)
		}
	}

	// Hashes link to the node page — the cache receipt.
	if !strings.Contains(body, "/p/demo/nodes/beef7654321") {
		t.Error("transcript does not link its step hashes to node pages")
	}
}

// TestReadOnlyServerRefusesMutations pins the contract a nil runner promises:
// the controls are absent from the page AND refused at the route, so a
// hand-built POST cannot do what the UI declines to offer.
func TestReadOnlyServerRefusesMutations(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, body := get(t, server, "/p/demo/jobs/build")
	if strings.Contains(body, "▶ Trigger") {
		t.Error("read-only server offers a trigger control")
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/p/demo/jobs/build/trigger", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("POST trigger on read-only server = %d, want 403", rec.Code)
	}
}

// TestCrossOriginMutationRefused covers the one defense a no-auth server can
// have: a page on another origin must not be able to aim a form at it.
func TestCrossOriginMutationRefused(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/p/demo/jobs/build/trigger", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST = %d, want 403", rec.Code)
	}
}

// TestUnknownPipelineAndRun404 checks the error page renders rather than
// echo's JSON default.
func TestUnknownPipelineAndRun404(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	for _, target := range []string{"/p/nope", "/p/demo/runs/nosuch", "/p/demo/jobs/nosuch"} {
		code, body := get(t, server, target)
		if code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, code)
		}

		if !strings.Contains(body, "<html") {
			t.Errorf("GET %s did not render an HTML error page", target)
		}
	}
}

// TestLiveStreamReplaysThenCloses covers the SSE contract: a finished run's
// events are delivered and the stream then closes, rather than holding a
// connection open against a table that can no longer change.
func TestLiveStreamReplaysThenCloses(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-2", "build", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-2", []store.RunEventRow{
		{Type: events.TypeStepStarted, JobName: "build", StepIndex: 0, StepName: "repo", StepKind: "get"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-2", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	done := make(chan string, 1)

	go func() {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/p/demo/runs/run-2/events", nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		done <- rec.Body.String()
	}()

	select {
	case body := <-done:
		if !strings.Contains(body, "event: event") {
			t.Errorf("stream delivered no events: %q", body)
		}

		if !strings.Contains(body, "event: done") {
			t.Errorf("stream did not close with a done event: %q", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SSE stream did not close for a finished run")
	}
}

// TestSearchFindsJobsAndRuns covers the jump palette's backing query.
func TestSearchFindsJobsAndRuns(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	code, body := get(t, server, "/p/demo/search?q=deploy")
	if code != http.StatusOK {
		t.Fatalf("search = %d", code)
	}

	if !strings.Contains(body, `"name":"deploy"`) {
		t.Errorf("search did not find the deploy job: %s", body)
	}

	_, filtered := get(t, server, "/p/demo/search?q=zzzznotathing")
	if strings.Contains(filtered, `"name":"deploy"`) {
		t.Error("search returned a non-matching job")
	}
}

// appendEvents persists a scripted event sequence, stamping each one so the
// ordering the reader depends on is real rather than incidental.
func appendEvents(t *testing.T, st *store.Store, runID string, rows []store.RunEventRow) {
	t.Helper()

	at := time.Now().UTC()

	for i, row := range rows {
		row.RunID = runID
		row.At = at.Add(time.Duration(i) * time.Millisecond)

		err := st.AppendRunEvent(context.Background(), row)
		if err != nil {
			t.Fatalf("AppendRunEvent: %v", err)
		}
	}
}

// writeFileRaw is os.WriteFile with the test's permissions, kept apart so the
// helper above reads as one line.
func writeFileRaw(path, body string) error {
	//nolint:wrapcheck // test fixture: the error is reported by the caller
	return os.WriteFile(path, []byte(body), 0o600)
}
