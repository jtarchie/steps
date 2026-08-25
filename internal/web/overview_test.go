package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// identically-named jobs with no way to tell them apart. Asserted on the
	// hint VALUE: "the body mentions infra" was already implied by the two
	// assertions above, so it could not fail.
	if !strings.Contains(body, `"name":"infra-job","hint":"infra"`) {
		t.Errorf("a cross-pipeline job hit does not name its pipeline: %s", body)
	}

	if !strings.Contains(body, `"hint":"infra running"`) {
		t.Errorf("a cross-pipeline run hit does not name its pipeline: %s", body)
	}

	// ...and the current pipeline's own hits do not, since repeating the page
	// you are already on is noise.
	_, own := get(t, server, "/p/infra/search?q=infra-job")
	if !strings.Contains(own, `"name":"infra-job","hint":""`) {
		t.Errorf("a hit in the current pipeline carries a redundant hint: %s", own)
	}
}

// TestSearchReachesOtherPipelinesPastTheCap is the defect the first version
// of this feature shipped with: the cap is global and the current pipeline
// went first, so its own runs — up to searchRunDepth of them — filled every
// slot and no neighbour was ever reached. Only a fixture with no history
// could miss it.
func TestSearchReachesOtherPipelinesPastTheCap(t *testing.T) {
	t.Parallel()

	server, pipelines := testPipelines(t, "app", "infra")

	for i := range searchHitLimit + 5 {
		id := fmt.Sprintf("app-run-%02d", i)

		err := pipelines[0].Store.StartRun(t.Context(), id, "app-job", t.TempDir())
		if err != nil {
			t.Fatalf("StartRun(%s): %v", id, err)
		}
	}

	_, body := get(t, server, "/p/app/search?q=job")

	if !strings.Contains(body, `"url":"/p/infra/jobs/infra-job"`) {
		t.Errorf("a pipeline with more than a screenful of runs never reaches its neighbour: %s", body)
	}
}

// TestSearchFiltersOnThePipelineFromEitherSide: the hint carries the slug
// only for OTHER pipelines, so filtering on what is RENDERED made the same
// query answer differently depending on which page it was typed on — and it
// stopped working on the one page where an operator is most likely to type
// that pipeline's name.
func TestSearchFiltersOnThePipelineFromEitherSide(t *testing.T) {
	t.Parallel()

	server, _ := testPipelines(t, "app", "infra")

	for _, from := range []string{"app", "infra"} {
		_, body := get(t, server, "/p/"+from+"/search?q=infra")

		if !strings.Contains(body, `"url":"/p/infra/jobs/infra-job"`) {
			t.Errorf("searching for infra from %s did not find its job: %s", from, body)
		}
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

	if !strings.Contains(body, `<span class="note spendwarn"`) {
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
	if strings.Contains(body, `<span class="note spendwarn"`) {
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
    max_turns: 17
    max_context_bytes: 400000

jobs:
  - name: review
    plan:
      - agent: reviewer
        max_turns: 8
        timeout: 20m
        messages: ["go"]
      - agent: reviewer
        messages: ["again"]
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

	// The step's own max_turns wins over the agent's...
	if !strings.Contains(body, ">8<") {
		t.Errorf("the job page does not show the resolved turn cap of 8: %s", body)
	}

	// ...and the second step, which states none, inherits the AGENT's. 17
	// rather than 30 on purpose: 30 is defaultMaxAgentTurns, so a fixture
	// using it cannot tell the agent tier from the built-in constant, and a
	// regression dropping the agent lookup entirely would pass.
	if !strings.Contains(body, ">17<") {
		t.Errorf("the job page does not show the agent's own turn cap: %s", body)
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

// TestLiveStreamCarriesWrappedUp is internal/web's standing rule asserted:
// anything the server draws for a finished step, the stream has to draw too.
// The marker was reload-only, so a reader watching an agent exhaust its turn
// budget saw the wrap-up answer arrive looking exactly like a confident one
// — and the fact was already in the map answerFor decodes, dropped on the
// floor beside the response it did take.
func TestLiveStreamCarriesWrappedUp(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-live-wrap", "build", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-live-wrap", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1, Status: "succeeded", Hash: "live-wrap-hash"},
	})

	mustRecordResult(t, pipeline, "live-wrap-hash", map[string]any{"response": "partial", "wrapped_up": true})

	err = pipeline.Store.FinishRun(ctx, "run-live-wrap", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	done := make(chan string, 1)

	go func() {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/p/demo/runs/run-live-wrap/events", nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		done <- rec.Body.String()
	}()

	select {
	case body := <-done:
		if !strings.Contains(body, `"wrapped_up":true`) {
			t.Errorf("the stream does not carry wrapped_up: %q", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SSE stream did not close for a finished run")
	}
}

// TestLiveStreamCarriesWrappedUpForACachedStep is the same invariant for the
// case a "finished" gate misses. An agent step that HITS THE CACHE publishes
// step.skipped carrying its node hash, and the server-rendered page reads
// that node's result for a skipped row exactly as it does for a finished one
// — so the badge appeared on reload and not live, which is the divergence
// this package's rule exists to forbid.
func TestLiveStreamCarriesWrappedUpForACachedStep(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-cached-wrap", "build", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-cached-wrap", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1},
		{Type: events.TypeStepSkipped, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1, Status: "skipped", Hash: "cached-wrap-hash", Text: "cached"},
	})

	mustRecordResult(t, pipeline, "cached-wrap-hash", map[string]any{"response": "partial", "wrapped_up": true})

	err = pipeline.Store.FinishRun(ctx, "run-cached-wrap", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	// The page a reader who reloaded sees.
	_, page := get(t, server, "/p/demo/runs/run-cached-wrap")
	if !strings.Contains(page, `<span class="note spendwarn"`) {
		t.Fatalf("the rendered page does not mark a cached wrapped-up step: %s", page)
	}

	done := make(chan string, 1)

	go func() {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/p/demo/runs/run-cached-wrap/events", nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		done <- rec.Body.String()
	}()

	select {
	case body := <-done:
		// ...and the stream a reader who watched sees.
		if !strings.Contains(body, `"wrapped_up":true`) {
			t.Errorf("the stream does not carry wrapped_up for a cached step: %q", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SSE stream did not close for a finished run")
	}
}

// TestGlobalPagesKeepAWayBack: the overview and /docs both sit above
// `/p/:pipeline`, so there is no current pipeline to build the shell's tabs,
// switcher and jump palette from. Rendered raw they emit "/p//..." links,
// every one of which 404s — and the palette's own error handling then
// swallows the HTML error page, so it shows nothing and says nothing. The
// page that lists the pipelines was the worst place to lose the way to them.
func TestGlobalPagesKeepAWayBack(t *testing.T) {
	t.Parallel()

	server, _ := testPipelines(t, "app", "infra")

	for _, page := range []string{"/", "/docs/README.md"} {
		_, body := get(t, server, page)

		for _, dead := range []string{`href="/p/"`, "/p//approvals", "/p//resources", "/p//search"} {
			if strings.Contains(body, dead) {
				t.Errorf("%s renders the dead link %q", page, dead)
			}
		}
	}
}
