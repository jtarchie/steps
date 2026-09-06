package web

import (
	"fmt"
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

		pipelines = append(pipelines, NewPipeline(name, path, cfg, st, events.New(nil)))
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
		err := seed.pipeline.Store.StartRun(ctx, seed.id, seed.job, t.TempDir(), "")
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

	err = other.StartRun(t.Context(), "unserved-1", "secret", t.TempDir(), "")
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

	err := pipelines[1].Store.StartRun(t.Context(), "infra-run", "infra-job", t.TempDir(), "")
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

		err := pipelines[0].Store.StartRun(t.Context(), id, "app-job", t.TempDir(), "")
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

	err := pipeline.Store.StartRun(ctx, "run-wrap", "build", t.TempDir(), "")
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

	err := pipeline.Store.StartRun(ctx, "run-ok", "build", t.TempDir(), "")
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

	// Three agents because the budget column has three states and they are not
	// interchangeable: a hosted agent takes budget.tokens, a cli agent takes
	// budget.usd (validation rejects each on the other), and an agent with
	// neither must read as uncapped rather than as zero.
	writeFile(t, path, `
agents:
  - name: reviewer
    source: { model: openrouter/qwen/qwen3.7-flash }
    max_turns: 17
    max_context_bytes: 400000
    budget: { tokens: 2000000 }
  - name: builder
    source: { model: "@claude/sonnet" }
    budget: { usd: 12.5 }
  - name: drifter
    source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
  - name: review
    plan:
      - agent: reviewer
        max_turns: 8
        timeout: 20m
        messages: ["go"]
      - agent: reviewer
        messages: ["again"]
      - agent: builder
        messages: ["build"]
      - agent: drifter
        messages: ["drift"]
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

	server, err := New([]*Pipeline{NewPipeline("demo", path, cfg, st, events.New(nil))}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return server
}

// agentSpendPipeline is a pipeline whose agent step has a budget, LOADED from
// disk so the config carries a real revision — the spend panel's ceiling is
// gated on the run's sha matching it, and a hand-built Config has none.
func agentSpendPipeline(t *testing.T) (*Server, *Pipeline) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "demo.yml")

	writeFile(t, path, `
agents:
  - name: reviewer
    source: { model: openrouter/qwen/qwen3.7-flash }
    budget: { tokens: 2000000 }
  - name: drifter
    source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
  - name: review
    plan:
      - agent: reviewer
        messages: ["go"]
      - agent: drifter
        messages: ["go"]
`)

	cfg, err := config.Load(path, "demo", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	st, err := store.OpenStore(filepath.Join(dir, ".steps", "state.db"), "demo")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	// Interned the way a real load interns it: runs.revision_id is resolved by
	// looking the sha up in pipeline_revisions, so a run started against a
	// revision nobody recorded reads back with no config sha at all.
	err = st.RecordRevision(t.Context(), cfg.Revision.SHA, cfg.Revision.Source)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	pipeline := NewPipeline("demo", path, cfg, st, events.New(nil))

	server, err := New([]*Pipeline{pipeline}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return server, pipeline
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

// TestJobPageShowsTheBudgetEachStepRunsUnder is the other half of "why did
// this step stop", and the half the page could not answer at all.
//
// A budget is the ceiling that binds when max_turns: does not, and for a CLI
// agent it is the ONLY one that binds: budget.tokens is a load error for a
// @cli source (nothing counts tokens until the subprocess exits) and a job
// budget is tokens-only, so a cli agent with no budget.usd is held by wall
// clock alone. A page that lists every other dial and omits this one is at its
// least useful for the step most able to spend.
func TestJobPageShowsTheBudgetEachStepRunsUnder(t *testing.T) {
	t.Parallel()

	server := agentJobPipeline(t)

	code, body := get(t, server, "/p/demo/jobs/review")
	if code != http.StatusOK {
		t.Fatalf("job page = %d", code)
	}

	// A hosted agent's ceiling, in the unit it is actually metered in.
	if !strings.Contains(body, "2,000,000 tokens") {
		t.Errorf("the job page does not show the hosted agent's token budget: %s", body)
	}

	// A cli agent's, in the other unit — and not silently rendered as tokens,
	// which is the mistake one shared column invites.
	if !strings.Contains(body, "$12.50") {
		t.Errorf("the job page does not show the cli agent's dollar budget: %s", body)
	}

	// And the state that matters most: no ceiling at all reads as a word. A 0
	// in a limit column says "nothing allowed", the exact opposite — and the
	// cell is matched with its delimiters because every token figure on the
	// page ends in "0 tokens".
	if strings.Contains(body, ">0 tokens<") {
		t.Errorf("an agent with no budget renders a zero ceiling: %s", body)
	}

	if !strings.Contains(body, "uncapped") {
		t.Errorf("an agent with no budget does not read as uncapped: %s", body)
	}
}

// TestJobPageSaysWhatATurnIs guards a word that means two different things in
// one column.
//
// A hosted agent's turn is one request/tool-execute round driven in this
// process. A CLI agent's is whatever the child reports as num_turns -- one per
// tool ROUND, pooled across every message: in the step -- which in practice
// runs an order of magnitude higher for the same work. The column cannot show
// two units, so it says which is which; without that, 30 beside a @claude
// source reads as generous and is not.
func TestJobPageSaysWhatATurnIs(t *testing.T) {
	t.Parallel()

	server := agentJobPipeline(t)

	_, body := get(t, server, "/p/demo/jobs/review")

	if !strings.Contains(body, "tool round") {
		t.Errorf("the Turns column does not say a cli counts turns differently: %s", body)
	}
}

// TestSpendRowMarksAStepThatFailedAfterItsLastAnswer keeps the spend panel
// from contradicting the run beside it.
//
// finish_reason is the PROVIDER's word about the last request that completed,
// and for a cli agent it is the last INVOCATION's -- so a step whose first
// message finished cleanly and then died against a pooled ceiling, before the
// second message was ever asked, records "success" on a step that failed. The
// panel rendered that verbatim next to a failed run.
//
// The provider's word is not overwritten: it is a fact about a request, and
// replacing it with steps' verdict on the step would put two vocabularies in
// one column. A second signal is added instead, which is what the neighbouring
// "truncated" decoration already does.
func TestSpendRowMarksAStepThatFailedAfterItsLastAnswer(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-ceiling", "build", t.TempDir(), "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-ceiling", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 2, StepName: "implementer", StepKind: "agent", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 2, StepName: "implementer", StepKind: "agent", StepID: 1, Status: "failed"},
	})

	mustRecordResult(t, pipeline, "ceiling-hash", map[string]any{"response": "a plan"})

	// What the run actually recorded: the child's first message succeeded and
	// reported its own spend, and the step then died on the pooled ceiling.
	err = pipeline.Store.RecordAgentUsage(ctx, store.AgentUsage{
		RunID: "run-ceiling", StepIndex: 2, StepName: "implementer", JobName: "build",
		NodeHash: "ceiling-hash", ModelReq: "opus", Total: 2333801, FinishReason: "success",
	})
	if err != nil {
		t.Fatalf("RecordAgentUsage: %v", err)
	}

	code, body := get(t, server, "/p/demo/runs/run-ceiling")
	if code != http.StatusOK {
		t.Fatalf("run page = %d", code)
	}

	// The premise, asserted rather than assumed: without the spend row there
	// is no column for either signal to be wrong in, and the test would pass
	// against a page that rendered neither.
	if !strings.Contains(body, "spendtable") {
		t.Fatalf("the run recorded no spend panel to assert about: %s", body)
	}

	// The whole cell, not either word alone: "failed" appears on the step row
	// as a CSS class and "success" is the reason being annotated, so matching
	// either separately passes against a page that draws neither signal.
	if !strings.Contains(body, "success \u2014 step failed") {
		t.Errorf("the spend panel reports success on a step that failed, with nothing saying otherwise: %s", body)
	}
}

// TestSpendRowLeavesASucceededStepAlone is the other half: the marker says
// something only if the ordinary case does not carry it. Every agent step that
// works reports a finish reason, so a marker that fires on all of them is
// noise on every run in the pipeline.
func TestSpendRowLeavesASucceededStepAlone(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-fine", "build", t.TempDir(), "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-fine", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 2, StepName: "implementer", StepKind: "agent", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 2, StepName: "implementer", StepKind: "agent", StepID: 1, Status: "succeeded"},
	})

	mustRecordResult(t, pipeline, "fine-hash", map[string]any{"response": "done"})

	err = pipeline.Store.RecordAgentUsage(ctx, store.AgentUsage{
		RunID: "run-fine", StepIndex: 2, StepName: "implementer", JobName: "build",
		NodeHash: "fine-hash", ModelReq: "opus", Total: 1200, FinishReason: "success",
	})
	if err != nil {
		t.Fatalf("RecordAgentUsage: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-fine")

	if strings.Contains(body, "step failed") {
		t.Errorf("a step that succeeded carries the failed-after marker: %s", body)
	}
}

// TestSpendPanelShowsTheCeilingWhenTheConfigStillMatches puts the number
// beside the ceiling it was spent against.
//
// A cost with nothing to read it against answers no question: $2.83 of
// unlimited and $2.83 of $3 are the same cell, and the run page is where a
// reader is standing when a step dies on a ceiling.
//
// The ceiling comes from the LIVE configuration, which is only the truth for a
// run that opened against it — so it is shown only when the run's recorded
// config sha still matches, and withheld otherwise. That gate is the whole
// design: a revision stores its source but not its include files, and
// internal/config can only load from a path, so a historical run's ceiling
// cannot honestly be reconstructed today. Silently rendering today's ceiling
// on last week's failure would be wrong exactly when the column is consulted.
func TestSpendPanelShowsTheCeilingWhenTheConfigStillMatches(t *testing.T) {
	t.Parallel()

	server, pipeline := agentSpendPipeline(t)
	ctx := t.Context()

	sha := pipeline.Config().Revision.SHA
	if sha == "" {
		t.Fatal("the loaded config carries no revision to match against")
	}

	err := pipeline.Store.StartRun(ctx, "run-cap", "review", t.TempDir(), sha)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-cap", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1, Status: "succeeded"},
	})

	mustRecordResult(t, pipeline, "cap-hash", map[string]any{"response": "done"})

	err = pipeline.Store.RecordAgentUsage(ctx, store.AgentUsage{
		RunID: "run-cap", StepIndex: 0, StepName: "reviewer", JobName: "review",
		NodeHash: "cap-hash", ModelReq: "opus", Total: 500000, FinishReason: "success",
	})
	if err != nil {
		t.Fatalf("RecordAgentUsage: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-cap")

	if !strings.Contains(body, "2,000,000") {
		t.Errorf("the spend panel does not show the ceiling the step ran under: %s", body)
	}
}

// TestAgentCeilingsRecordsUncappedAgentsToo is the producing half of that
// doctrine: absence from the map has to keep meaning "unresolved", so an agent
// the configuration says has no ceiling is present with an empty value rather
// than left out. Dropping it makes every uncapped agent indistinguishable from
// a step whose name resolves to no agent at all.
func TestAgentCeilingsRecordsUncappedAgentsToo(t *testing.T) {
	t.Parallel()

	_, pipeline := agentSpendPipeline(t)
	cfg := pipeline.Config()

	ceilings, drifted := agentCeilings(cfg, "review", cfg.Revision.SHA)
	if drifted {
		t.Fatal("the loaded config reports as drifted against its own sha")
	}

	if got := ceilings["reviewer"]; got != "2,000,000 tokens" {
		t.Errorf("ceilings[reviewer] = %q, want the agent's budget", got)
	}

	got, known := ceilings["drifter"]
	if !known {
		t.Error("an agent with no budget is absent from the map, which reads as unresolved")
	}

	if got != "" {
		t.Errorf("ceilings[drifter] = %q, want empty — it has no ceiling", got)
	}
}

// TestSpendPanelNeverCallsAnUnknownCeilingUncapped is the doctrine of this
// column, asserted against the case that keeps finding a way around it.
//
// Ceilings are the AGENT's, and agent_usage records a STEP name. Those agree
// for an ordinary step and disagree for an across: cell — which renames itself
// "<agent> [k=v]" unless its name: template references an axis, in which case
// it takes an arbitrary name matching neither. A lookup miss that renders as
// "uncapped" states the opposite of the truth for a step that has a ceiling,
// on the run where the ceiling is why it died. Unknown must read as unknown.
func TestSpendPanelNeverCallsAnUnknownCeilingUncapped(t *testing.T) {
	t.Parallel()

	view := runView{
		Ceilings: map[string]string{"reviewer": "$3.00", "drifter": ""},
	}

	if got, known := view.ceilingFor("reviewer"); !known || got != "$3.00" {
		t.Errorf("ceilingFor(agent) = %q/%v, want the agent's own ceiling", got, known)
	}

	// A cell that renamed itself resolves through the agent underneath it.
	if got, known := view.ceilingFor("reviewer [shard=a]"); !known || got != "$3.00" {
		t.Errorf("ceilingFor(cell) = %q/%v, want the agent's ceiling", got, known)
	}

	// An agent the config says has no ceiling is KNOWN to be uncapped.
	if got, known := view.ceilingFor("drifter"); !known || got != "" {
		t.Errorf("ceilingFor(uncapped agent) = %q/%v, want known and empty", got, known)
	}

	// A name that resolves to no agent at all is not uncapped, it is unknown —
	// an across: cell whose name: template names an axis takes a name matching
	// neither the agent nor the "[k=v]" shape.
	if _, known := view.ceilingFor("review-shard-a"); known {
		t.Error("a step whose ceiling could not be resolved reports as uncapped")
	}
}

// TestSpendPanelWithholdsTheCeilingAfterAnEdit is the half that keeps the
// column honest, and the reason it exists at all.
//
// "What was this capped at when it failed?" is the question the column is
// consulted for, so answering it with a number from a configuration the run
// never saw is worse than answering nothing. A run whose sha does not match
// says the config changed instead.
func TestSpendPanelWithholdsTheCeilingAfterAnEdit(t *testing.T) {
	t.Parallel()

	server, pipeline := agentSpendPipeline(t)
	ctx := t.Context()

	// Interned, so the run genuinely carries the older sha rather than none —
	// runs.revision_id resolves by lookup, and an unrecorded sha reads back
	// empty, which is a different state (see agentCeilings).
	err := pipeline.Store.RecordRevision(ctx, "a-sha-from-an-older-file", "agents: []\n")
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	err = pipeline.Store.StartRun(ctx, "run-old", "review", t.TempDir(), "a-sha-from-an-older-file")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-old", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1, Status: "succeeded"},
	})

	mustRecordResult(t, pipeline, "old-hash", map[string]any{"response": "done"})

	err = pipeline.Store.RecordAgentUsage(ctx, store.AgentUsage{
		RunID: "run-old", StepIndex: 0, StepName: "reviewer", JobName: "review",
		NodeHash: "old-hash", ModelReq: "opus", Total: 500000, FinishReason: "success",
	})
	if err != nil {
		t.Fatalf("RecordAgentUsage: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-old")

	if strings.Contains(body, "2,000,000") {
		t.Errorf("the spend panel shows a ceiling from a config this run never opened against: %s", body)
	}

	if !strings.Contains(body, "config changed") {
		t.Errorf("the spend panel withholds the ceiling without saying why: %s", body)
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

	err := pipeline.Store.StartRun(ctx, "run-live-wrap", "build", "", "")
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

	stream := sseHTML(streamOf(t, server, "/p/demo/runs/run-live-wrap/events"))
	if !strings.Contains(stream, `<span class="note spendwarn"`) {
		t.Errorf("the stream does not mark a wrapped-up step: %q", stream)
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

	err := pipeline.Store.StartRun(ctx, "run-cached-wrap", "build", "", "")
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

	// ...and the stream a reader who watched sees.
	stream := sseHTML(streamOf(t, server, "/p/demo/runs/run-cached-wrap/events"))
	if !strings.Contains(stream, `<span class="note spendwarn"`) {
		t.Errorf("the stream does not mark a cached wrapped-up step: %q", stream)
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

// TestSpendPanelCeilingReachesAnAcrossCell crosses the seam the ceiling
// column is built on: ceilings are keyed by the AGENT a step resolves through
// and spend is recorded under the name the step is KNOWN by, and an across:
// cell is the case where those two are different strings (config.nameCell
// renames it to "<agent> [k=v]"). A missed lookup does not render as a blank
// — it falls through to "uncapped", which is the opposite of the truth and
// the one word this column exists to avoid printing.
func TestSpendPanelCeilingReachesAnAcrossCell(t *testing.T) {
	t.Parallel()

	server, pipeline := agentSpendPipeline(t)
	ctx := t.Context()

	sha := pipeline.Config().Revision.SHA

	err := pipeline.Store.StartRun(ctx, "run-cell", "review", t.TempDir(), sha)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-cell", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "reviewer [shard=a]", StepKind: "agent", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "reviewer [shard=a]", StepKind: "agent", StepID: 1,
			Status: "succeeded"},
	})

	mustRecordResult(t, pipeline, "cell-hash", map[string]any{"response": "done"})

	err = pipeline.Store.RecordAgentUsage(ctx, store.AgentUsage{
		RunID: "run-cell", StepIndex: 0, StepName: "reviewer [shard=a]", JobName: "review",
		NodeHash: "cell-hash", ModelReq: "opus", Total: 500000, FinishReason: "success",
	})
	if err != nil {
		t.Fatalf("RecordAgentUsage: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-cell")

	if !strings.Contains(body, "2,000,000") {
		t.Errorf("a matrix cell's row does not show the ceiling its agent runs under: %s", body)
	}

	if strings.Contains(body, "uncapped") {
		t.Errorf("a capped matrix cell is reported as uncapped: %s", body)
	}
}

// TestSpendPanelDoesNotBlameASiblingCell: every cell of an across: and every
// member of an ensemble: is handed the block's OWN plan index, so a `failed`
// set keyed on the index alone marked all of them when one failed — the
// "step failed" annotation drawn beside four rows that succeeded, which the
// marker is worthless if it does.
func TestSpendPanelDoesNotBlameASiblingCell(t *testing.T) {
	t.Parallel()

	server, pipeline := agentSpendPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-cells", "review", t.TempDir(), "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-cells", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "reviewer [shard=a]", StepKind: "agent", StepID: 1},
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "reviewer [shard=b]", StepKind: "agent", StepID: 2},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "reviewer [shard=a]", StepKind: "agent", StepID: 1,
			Status: "succeeded"},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "reviewer [shard=b]", StepKind: "agent", StepID: 2,
			Status: "failed", Text: "the model gave up"},
	})

	for _, cell := range []string{"reviewer [shard=a]", "reviewer [shard=b]"} {
		mustRecordResult(t, pipeline, "hash-"+cell, map[string]any{"response": "done"})

		err = pipeline.Store.RecordAgentUsage(ctx, store.AgentUsage{
			RunID: "run-cells", StepIndex: 0, StepName: cell, JobName: "review",
			NodeHash: "hash-" + cell, ModelReq: "opus", Total: 10, FinishReason: "stop",
		})
		if err != nil {
			t.Fatalf("RecordAgentUsage: %v", err)
		}
	}

	_, body := get(t, server, "/p/demo/runs/run-cells")

	// The exact cell text, not the bare words: `class="step failed"` on the
	// transcript row above it matches a looser search.
	if marked := strings.Count(body, "— step failed"); marked != 1 {
		t.Errorf("one cell failed and %d spend rows say so: %s", marked, body)
	}
}

// TestSpendPanelSaysNothingWithoutAFinishReason: a provider that reported
// nothing gets a row of zeros on purpose (saveAgentUsage), and a step killed
// by timeout: is the common producer of one. Annotating that empty cell drew
// a dangling " — step failed" under a tooltip quoting a word never said.
func TestSpendPanelSaysNothingWithoutAFinishReason(t *testing.T) {
	t.Parallel()

	server, pipeline := agentSpendPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-quiet", "review", t.TempDir(), "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-quiet", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1,
			Status: "aborted", Text: "context deadline exceeded"},
	})

	mustRecordResult(t, pipeline, "quiet-hash", map[string]any{"response": "done"})

	err = pipeline.Store.RecordAgentUsage(ctx, store.AgentUsage{
		RunID: "run-quiet", StepIndex: 0, StepName: "reviewer", JobName: "review",
		NodeHash: "quiet-hash", ModelReq: "opus", Total: 10,
	})
	if err != nil {
		t.Fatalf("RecordAgentUsage: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-quiet")

	if strings.Contains(body, "— step failed") {
		t.Errorf("a row with no finish reason is annotated as if the provider had said one: %s", body)
	}
}
