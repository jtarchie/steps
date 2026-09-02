package main

// The web UI, end to end: a real run through the whole stack (CLI → config →
// merkle → workspace → agent conversation → store), then the pages that read
// it back.
//
// It lives in the root package for the same reason every other e2e test does:
// only main's run() spans the whole stack, and source.endpoint: is the sole
// injection point for a scripted model. What it adds over the pure web tests
// in internal/web is that nothing here is hand-fed — the events, the node
// results, and the agent transcript are all whatever an actual run produced.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/web"
)

// TestWebUIRendersARealAgentRun runs an agent pipeline against the fake
// provider, then asserts the UI shows what the run actually did: the steps in
// plan order, the agent's turns and tool calls beneath its step, and the
// verdict it reached.
func TestWebUIRendersARealAgentRun(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()
	fake := newFakeLLM(t, happyPathScript()...)
	path := e2ePipeline(t, dir, fake.URL, "")

	mustRun(t, "run", path, "--job", "build")

	server, pipeline := webServerFor(t, path)

	// The board lists the job with its recorded outcome.
	code, board := webGet(t, server, "/p/"+pipeline.Slug)
	if code != http.StatusOK {
		t.Fatalf("jobs board = %d", code)
	}

	if !strings.Contains(board, "build") {
		t.Error("jobs board does not list the job that ran")
	}

	runs, err := pipeline.Store.ListRuns(t.Context(), "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(runs) == 0 {
		t.Fatal("the run recorded no history row")
	}

	run := runs[0]
	if run.Status != "succeeded" || run.FinishedAt.IsZero() {
		t.Errorf("recorded run = %+v, want succeeded with a finish time", run)
	}

	code, body := webGet(t, server, "/p/"+pipeline.Slug+"/runs/"+run.ID)
	if code != http.StatusOK {
		t.Fatalf("run transcript = %d: %s", code, body)
	}

	assertAgentTranscript(t, body)
}

// assertAgentTranscript checks a run transcript carries the plan's steps and
// the agent's own conversation traffic — the two things the terminal only
// ever showed as it scrolled past.
func assertAgentTranscript(t *testing.T, body string) {
	t.Helper()

	for _, want := range []string{"repo", "prepare", "reviewer", "results"} {
		if !strings.Contains(body, `class="name">`+want+`</span>`) {
			t.Errorf("transcript missing step %q", want)
		}
	}

	for _, want := range []string{"read_file", "write_file", "verdict"} {
		if !strings.Contains(body, want) {
			t.Errorf("transcript missing agent tool call %q", want)
		}
	}

	if !strings.Contains(body, "approve") {
		t.Error("transcript does not show the verdict the agent reached")
	}
}

// TestWebUIShowsCachedStepsAsSkipped is the product's central mechanism seen
// through the UI: rerun the same pipeline and the second run's transcript
// must distinguish what it replayed from what it paid for.
func TestWebUIShowsCachedStepsAsSkipped(t *testing.T) {
	dir := t.TempDir()
	pipelinePath := filepath.Join(dir, "cached.yml")

	writeE2EPipeline(t, pipelinePath, `
jobs:
- name: build
  plan:
  - task: compile
    run: echo compiling
  - task: verify
    run: echo verifying
`)

	mustRun(t, "run", pipelinePath, "--job", "build")
	mustRun(t, "run", pipelinePath, "--job", "build")

	server, pipeline := webServerFor(t, pipelinePath)

	runs, err := pipeline.Store.ListRuns(t.Context(), "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}

	// runs is newest-first: the second invocation is the cached one.
	_, cached := webGet(t, server, "/p/"+pipeline.Slug+"/runs/"+runs[0].ID)
	if !strings.Contains(cached, "unchanged — replayed from cache") {
		t.Error("the rerun's transcript does not say its steps were replayed")
	}

	if !strings.Contains(cached, `class="step skipped`) {
		t.Error("the rerun's transcript does not render replayed steps as skipped")
	}

	_, first := webGet(t, server, "/p/"+pipeline.Slug+"/runs/"+runs[1].ID)
	if strings.Contains(first, "unchanged — replayed from cache") {
		t.Error("the first run's transcript claims it replayed steps it actually ran")
	}

	if !strings.Contains(first, `class="step passed`) {
		t.Error("the first run's transcript does not render its steps as executed")
	}
}

// TestWebUIFailedRunOpensWithTheError covers triage: a failed run must lead
// with what broke, and mark the failing step rather than leaving a reader to
// expand every step looking for it.
func TestWebUIFailedRunOpensWithTheError(t *testing.T) {
	dir := t.TempDir()
	pipelinePath := filepath.Join(dir, "failing.yml")

	writeE2EPipeline(t, pipelinePath, `
jobs:
- name: build
  plan:
  - task: ok
    run: echo fine
  - task: boom
    run: exit 3
`)

	err := run([]string{"run", pipelinePath, "--job", "build"})
	if err == nil {
		t.Fatal("expected the failing pipeline to fail")
	}

	server, pipeline := webServerFor(t, pipelinePath)

	runs, listErr := pipeline.Store.ListRuns(t.Context(), "build", 10)
	if listErr != nil || len(runs) == 0 {
		t.Fatalf("ListRuns: %v (%d runs)", listErr, len(runs))
	}

	if runs[0].Status != "failed" {
		t.Errorf("recorded status = %q, want failed", runs[0].Status)
	}

	_, body := webGet(t, server, "/p/"+pipeline.Slug+"/runs/"+runs[0].ID)

	if !strings.Contains(body, "errblock") {
		t.Error("failed run does not lead with the job error")
	}

	if !strings.Contains(body, `class="step failed`) {
		t.Error("failed run does not mark the failing step")
	}

	if !strings.Contains(body, "exit status 3") {
		t.Error("failed run does not show what the failing command reported")
	}

	// The plan wraps a step's error and returns it as the job's, so the two
	// are the same text. It appears once, as the headline — printing it again
	// under the step it names is noise on the page a reader reaches while
	// triaging.
	if strings.Count(body, "exit status 3") != 1 {
		t.Errorf("the failure text appears %d times, want once", strings.Count(body, "exit status 3"))
	}

	// The step that succeeded before it is still shown, so the run reads as a
	// sequence rather than as its failure alone.
	if !strings.Contains(body, `class="name">ok</span>`) {
		t.Error("failed run does not show the steps that ran before the failure")
	}
}

// webServerFor opens a read-only server over an already-run pipeline, the way
// `steps web --read-only` would.
func webServerFor(t *testing.T, pipelinePath string) (*web.Server, *web.Pipeline) {
	t.Helper()

	cfg, err := config.LoadConfig(pipelinePath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := store.OpenStore(statePath(pipelinePath, ""), pipelineName(pipelinePath))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	pipeline := web.NewPipeline(web.Slugify(pipelinePath), pipelinePath, cfg, st, events.New(nil))

	server, err := web.New([]*web.Pipeline{pipeline}, nil)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	return server, pipeline
}

// webPipelineWithVars is webServerFor's pipeline half, loaded under the vars
// the daemon was started with — which is what makes a --vars-file part of
// the configuration a reload compares against.
func webPipelineWithVars(t *testing.T, pipelinePath string, vars VarFlags) *web.Pipeline {
	t.Helper()

	slug := web.Slugify(pipelinePath)

	cfg, err := vars.Load(pipelinePath, slug)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	st, err := store.OpenStore(statePath(pipelinePath, ""), slug)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	err = recordRevision(t.Context(), st, cfg)
	if err != nil {
		t.Fatalf("recordRevision: %v", err)
	}

	return web.NewPipeline(slug, pipelinePath, cfg, st, events.New(nil))
}

// webGet performs a GET against the server and returns status and body.
func webGet(t *testing.T, server *web.Server, target string) (int, string) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Code, rec.Body.String()
}

// writeE2EPipeline writes a pipeline fixture.
func writeE2EPipeline(t *testing.T, path, body string) {
	t.Helper()

	err := os.WriteFile(path, []byte(body), 0o600)
	if err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
