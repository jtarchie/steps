package web

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// testPipelines builds a server over several pipelines sharing one state
// file, which is what `steps web app.yml infra.yml --state shared.db`
// produces. Each gets one job named after itself, so a page can be checked
// for having reached the right one.
func testPipelines(t *testing.T, names ...string) (*Server, []*Pipeline) {
	t.Helper()

	dir := t.TempDir()
	statePath := filepath.Join(dir, ".steps", "shared.db")
	pipelines := make([]*Pipeline, 0, len(names))

	for _, name := range names {
		path := filepath.Join(dir, name+".yml")
		writeFile(t, path, `
jobs:
  - name: `+name+`-job
    plan:
      - task: work
        run: "true"
`)

		cfg, err := config.LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig(%s): %v", name, err)
		}

		st, err := store.OpenStore(statePath, name)
		if err != nil {
			t.Fatalf("OpenStore(%s): %v", name, err)
		}

		t.Cleanup(func() { _ = st.Close() })

		pipelines = append(pipelines, &Pipeline{
			Slug: name, Path: path, Cfg: cfg, Store: st, Bus: events.New(nil),
		})
	}

	server, err := New(pipelines, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return server, pipelines
}

// TestRootRedirectsForASinglePipeline is the common case, and the reason the
// root adapts rather than always becoming a feed: with one pipeline served,
// an overview is a list of one and a click in front of everything useful.
func TestRootRedirectsForASinglePipeline(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	code, _ := get(t, server, "/")
	if code != http.StatusFound {
		t.Fatalf("GET / = %d, want a redirect straight to the only pipeline", code)
	}
}

// TestRootListsWhatTheStateFileHolds: with several pipelines served, the root
// stops guessing. It used to redirect to whichever slug sorted first, which
// is an arbitrary answer to a question the operator did not ask.
func TestRootListsEveryServedPipeline(t *testing.T) {
	t.Parallel()

	server, _ := testPipelines(t, "app", "infra")

	code, body := get(t, server, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / = %d, want an overview rather than a redirect", code)
	}

	for _, slug := range []string{"app", "infra"} {
		if !strings.Contains(body, `/p/`+slug) {
			t.Errorf("the overview does not link to /p/%s", slug)
		}
	}
}

// TestRootFeedSpansPipelines is the view #85 exists for: one feed, newest
// first, every row saying which pipeline it belongs to. A row that cannot
// name its pipeline is a row nobody can follow.
func TestRootFeedSpansPipelines(t *testing.T) {
	t.Parallel()

	server, pipelines := testPipelines(t, "app", "infra")
	ctx := t.Context()

	for _, seed := range []struct {
		pipeline *Pipeline
		id       string
		job      string
	}{
		{pipelines[0], "app-1", "app-job"},
		{pipelines[1], "infra-1", "infra-job"},
		{pipelines[0], "app-2", "app-job"},
	} {
		err := seed.pipeline.Store.StartRun(ctx, seed.id, seed.job, t.TempDir())
		if err != nil {
			t.Fatalf("StartRun(%s): %v", seed.id, err)
		}
	}

	_, body := get(t, server, "/")

	for _, id := range []string{"app-1", "infra-1", "app-2"} {
		if !strings.Contains(body, id) {
			t.Errorf("the feed is missing run %s", id)
		}
	}

	// Newest first, and interleaved — ordering by pipeline would put both of
	// app's runs together and still contain all three ids.
	first := strings.Index(body, "app-2")
	second := strings.Index(body, "infra-1")
	third := strings.Index(body, "app-1")

	if first >= second || second >= third {
		t.Errorf("feed order is app-2@%d, infra-1@%d, app-1@%d; want newest first", first, second, third)
	}

	// Each run has to be reachable, which means the row carries its pipeline.
	for _, want := range []string{"/p/app/runs/app-2", "/p/infra/runs/infra-1"} {
		if !strings.Contains(body, want) {
			t.Errorf("the feed does not link %s", want)
		}
	}
}

// TestRootFeedIgnoresUnservedPipelines: a state file may hold a pipeline this
// process was not given. Showing its runs would put rows on the page with no
// route behind them, and — because the feed is bounded — would crowd out runs
// of pipelines the operator IS looking at.
func TestRootFeedIgnoresUnservedPipelines(t *testing.T) {
	t.Parallel()

	server, pipelines := testPipelines(t, "app", "infra")

	// A third pipeline writing into the same file, served by nobody here.
	other, err := store.OpenStore(filepath.Join(filepath.Dir(pipelines[0].Path), ".steps", "shared.db"), "unserved")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = other.Close() })

	err = other.StartRun(t.Context(), "unserved-1", "secret", t.TempDir())
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	_, body := get(t, server, "/")

	if strings.Contains(body, "unserved-1") {
		t.Error("the feed shows a run from a pipeline this process does not serve")
	}
}

// TestSearchSpansServedPipelines: the palette used to answer only about the
// pipeline whose page you happened to be on, so finding a job in the other
// one meant knowing it existed and navigating there first — the jump palette
// could not jump anywhere it had not already taken you.
func TestSearchSpansServedPipelines(t *testing.T) {
	t.Parallel()

	server, pipelines := testPipelines(t, "app", "infra")

	err := pipelines[1].Store.StartRun(t.Context(), "infra-run", "infra-job", t.TempDir())
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// Searching from app's page for something only infra has.
	code, body := get(t, server, "/p/app/search?q=infra-job")
	if code != http.StatusOK {
		t.Fatalf("search = %d", code)
	}

	if !strings.Contains(body, `"url":"/p/infra/jobs/infra-job"`) {
		t.Errorf("search from app did not find infra's job: %s", body)
	}

	if !strings.Contains(body, `"url":"/p/infra/runs/infra-run"`) {
		t.Errorf("search from app did not find infra's run: %s", body)
	}

	// A hit in another pipeline has to say so, or the palette offers two
	// identically-named jobs with no way to tell them apart.
	if !strings.Contains(body, `infra`) {
		t.Error("a cross-pipeline hit does not name its pipeline")
	}
}

// TestSearchPrefersTheCurrentPipeline pins the ordering the result cap makes
// matter. Results are bounded, so a busy neighbour must not push the jobs of
// the pipeline you are actually looking at off the end.
func TestSearchPrefersTheCurrentPipeline(t *testing.T) {
	t.Parallel()

	server, _ := testPipelines(t, "app", "infra")

	// "-job" matches both pipelines' jobs.
	_, body := get(t, server, "/p/infra/search?q=-job")

	own := strings.Index(body, `"/p/infra/jobs/infra-job"`)
	other := strings.Index(body, `"/p/app/jobs/app-job"`)

	if own < 0 || other < 0 {
		t.Fatalf("search did not return both jobs: %s", body)
	}

	if own > other {
		t.Errorf("the current pipeline's job ranked after the other one's (%d vs %d)", own, other)
	}
}

// TestRunPageMarksAWrappedUpStep: a step that ran out of turns and was asked
// to answer from what it had is a DEGRADED answer, and afterwards it is
// indistinguishable from a confident one. The runner records wrapped_up for
// exactly that reason (see agentResultRecord) and the page never showed it,
// so the one signal that tells "the model had nothing more to say" from "the
// model was cut off" existed only in the database.
func TestRunPageMarksAWrappedUpStep(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-wrap", "build", t.TempDir())
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-wrap", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1, Status: "succeeded", Hash: "wrap-hash"},
	})

	mustRecordResult(t, pipeline, "wrap-hash", map[string]any{"response": "partial", "turns": 30, "wrapped_up": true})

	code, body := get(t, server, "/p/demo/runs/run-wrap")
	if code != http.StatusOK {
		t.Fatalf("run page = %d", code)
	}

	if !strings.Contains(body, "stopped early") {
		t.Errorf("the run page does not mark a wrapped-up step: %s", body)
	}
}

// TestRunPageLeavesAnOrdinaryStepAlone is the other half: the marker means
// something only if a step that finished on its own does not carry it.
func TestRunPageLeavesAnOrdinaryStepAlone(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-ok", "build", t.TempDir())
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-ok", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1, Status: "succeeded", Hash: "ok-hash"},
	})

	mustRecordResult(t, pipeline, "ok-hash", map[string]any{"response": "all done", "turns": 4})

	_, body := get(t, server, "/p/demo/runs/run-ok")
	if strings.Contains(body, "stopped early") {
		t.Error("a step that finished on its own was marked as stopped early")
	}
}

// mustRecordResult records a node carrying one agent step's result, which is
// where the runner puts wrapped_up.
func mustRecordResult(t *testing.T, pipeline *Pipeline, hash string, result map[string]any) {
	t.Helper()

	err := pipeline.Store.RecordNode(t.Context(),
		store.NodeRecord{Hash: hash, Kind: "agent", Resource: "reviewer"},
		"build", "succeeded", result, nil)
	if err != nil {
		t.Fatalf("RecordNode(%s): %v", hash, err)
	}
}

// agentJobPipeline is one job whose plan runs an agent, with the dials split
// across the agent and the step the way a real pipeline splits them.
func agentJobPipeline(t *testing.T) *Server {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "demo.yml")

	writeFile(t, path, `
agents:
  - name: reviewer
    source: { model: openrouter/qwen/qwen3.7-flash }
    max_turns: 30
    max_context_bytes: 400000

jobs:
  - name: review
    plan:
      - agent: reviewer
        max_turns: 8
        timeout: 20m
        messages: ["go"]
`)

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := store.OpenStore(filepath.Join(dir, ".steps", "state.db"), "demo")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	server, err := New([]*Pipeline{{
		Slug: "demo", Path: path, Cfg: cfg, Store: st, Bus: events.New(nil),
	}}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return server
}

// TestJobPageShowsResolvedAgentDials answers "why did this step stop at N
// turns" on the page, rather than by cross-referencing the step, the agent,
// and a default constant in Go. The values shown are the RESOLVED ones, which
// is the whole point — the step's 8 turns win over the agent's 30, and the
// agent's context ceiling applies because the step states none.
func TestJobPageShowsResolvedAgentDials(t *testing.T) {
	t.Parallel()

	server := agentJobPipeline(t)

	code, body := get(t, server, "/p/demo/jobs/review")
	if code != http.StatusOK {
		t.Fatalf("job page = %d", code)
	}

	// The step's own max_turns, not the agent's.
	if !strings.Contains(body, ">8<") {
		t.Errorf("the job page does not show the resolved turn cap of 8: %s", body)
	}

	if strings.Contains(body, ">30<") {
		t.Error("the job page shows the agent's 30 turns, which this step overrode")
	}

	// Inherited from the agent, since the step states none.
	if !strings.Contains(body, "400,000") {
		t.Error("the job page does not show the inherited context ceiling")
	}

	if !strings.Contains(body, "20m") {
		t.Error("the job page does not show the step's deadline")
	}
}

// TestJobPageOmitsDialsWithoutAgents keeps the section off a job that has no
// agent step, rather than rendering an empty table on every ordinary job.
func TestJobPageOmitsDialsWithoutAgents(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, body := get(t, server, "/p/demo/jobs/build")
	if strings.Contains(body, "Agent dials") {
		t.Error("a job with no agent steps renders the dials section")
	}
}
