package web

import (
	"context"
	"fmt"
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

	st, err := store.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	pipeline := NewPipeline("demo", path, cfg, st, events.New(nil))

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
	if !strings.Contains(deployBody, "must have passed for") {
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
		{Type: events.TypeJobStarted, StepIndex: -1},
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "repo", StepKind: "get"},
		{Type: events.TypeStepSkipped, StepIndex: 0, StepName: "repo", StepKind: "get", Status: "skipped", Hash: "cafe1234567", Text: "unchanged — replayed from cache"},
		{Type: events.TypeStepStarted, StepIndex: 1, StepName: "review", StepKind: "agent"},
		{Type: events.TypeAgentText, StepIndex: 1, StepName: "review", Text: "Reading the diff first."},
		{Type: events.TypeAgentCall, StepIndex: 1, StepName: "review", Name: "read_file", Detail: `{"path":"main.go"}`},
		{Type: events.TypeAgentSubagent, StepIndex: 1, StepName: "review", Name: "test-runner", Text: "run the suite", Status: "depth:1"},
		{Type: events.TypeStepFinished, StepIndex: 1, StepName: "review", StepKind: "agent", Status: "succeeded", Hash: "beef7654321", DurationMS: 4200},
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
//
// The server here has a RUNNER, which the test lacked until mutation testing
// showed why it mattered: without one every mutation is refused as read-only,
// so a 403 proved nothing about the origin check and the whole middleware
// could be deleted with this test still green. The companion cases below are
// the other half — a defense that refuses everything is not a defense either.
func TestCrossOriginMutationRefused(t *testing.T) {
	t.Parallel()

	server, _ := mutableServer(t)

	if code := postWithOrigin(t, server, "/p/demo/jobs/build/trigger", "https://evil.example"); code != http.StatusForbidden {
		t.Errorf("cross-origin POST = %d, want 403", code)
	}
}

// TestSameOriginMutationAllowed: the check compares against the host it was
// sent to, so the UI's own forms must pass. A mutant that refused everything
// would otherwise look exactly like a working defense.
func TestSameOriginMutationAllowed(t *testing.T) {
	t.Parallel()

	server, _ := mutableServer(t)

	if code := postWithOrigin(t, server, "/p/demo/jobs/build/trigger", "http://example.com"); code == http.StatusForbidden {
		t.Error("the server refused a form served from its own origin")
	}
}

// TestOriginlessMutationAllowed: a request with no Origin at all is a curl or
// a CLI, not a browser being aimed at this port by another page. Refusing it
// would break every scripted trigger while stopping no attack.
func TestOriginlessMutationAllowed(t *testing.T) {
	t.Parallel()

	server, _ := mutableServer(t)

	if code := postWithOrigin(t, server, "/p/demo/jobs/build/trigger", ""); code == http.StatusForbidden {
		t.Error("the server refused a request carrying no Origin")
	}
}

// TestSafeMethodsSkipTheOriginCheck: reads are not mutations, and the check
// is about what another page can make this server DO. A mutant that moved a
// safe method out of that list would make the UI unusable from anywhere the
// browser sets an Origin.
func TestSafeMethodsSkipTheOriginCheck(t *testing.T) {
	t.Parallel()

	server, _ := mutableServer(t)

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req := httptest.NewRequestWithContext(t.Context(), method, "/p/demo", nil)
		req.Header.Set("Origin", "https://evil.example")

		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code == http.StatusForbidden {
			t.Errorf("%s from another origin was refused; safe methods are not mutations", method)
		}
	}
}

// mutableServer is testPipeline's server with a runner attached, so a refusal
// can only have come from the origin check rather than from --read-only.
func mutableServer(t *testing.T) (*Server, *Pipeline) {
	t.Helper()

	_, pipeline := testPipeline(t)

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return server, pipeline
}

// postWithOrigin sends a POST carrying origin (or none, when empty) and
// returns the status. The Host is example.com, which is what httptest sets
// and what "same origin" therefore means here.
func postWithOrigin(t *testing.T, server *Server, target, origin string) int {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Code
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
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "repo", StepKind: "get"},
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

// TestGuardSkippedStepDoesNotStayRunning is the reader's half of the identity
// a skip event carries. A when:-guarded step announces itself and is only
// then skipped, so both events name the same step id — and the transcript has
// to fold them into ONE closed row. Under a fresh id per event the page held
// the start open forever: a job reported succeeded while one of its steps
// still ticked.
func TestGuardSkippedStepDoesNotStayRunning(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-guard", "build", "/tmp/ws")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-guard", []store.RunEventRow{
		{Type: events.TypeJobStarted, StepIndex: -1},
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "compile", StepKind: "task", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "compile", StepKind: "task", StepID: 1, Status: "succeeded", DurationMS: 12},
		{Type: events.TypeStepStarted, StepIndex: 1, StepName: "publish", StepKind: "put", StepID: 2},
		{Type: events.TypeStepSkipped, StepIndex: 1, StepName: "publish", StepKind: "put", StepID: 2, Status: "skipped", Text: "when: guard was false"},
		{Type: events.TypeJobFinished, StepIndex: -1, Status: "succeeded"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-guard", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	code, body := get(t, server, "/p/demo/runs/run-guard")
	if code != http.StatusOK {
		t.Fatalf("GET run = %d: %s", code, body)
	}

	if strings.Contains(body, `class="step running`) {
		t.Error("a finished run renders a step as still running")
	}

	if got := strings.Count(body, `class="step skipped`); got != 1 {
		t.Errorf("guarded step rendered %d times as skipped, want exactly 1", got)
	}
}

// writeFileRaw is os.WriteFile with the test's permissions, kept apart so the
// helper above reads as one line.
func writeFileRaw(path, body string) error {
	//nolint:wrapcheck // test fixture: the error is reported by the caller
	return os.WriteFile(path, []byte(body), 0o600)
}

// TestRunPageTitleCarriesStatus covers the background-tab case: the title and
// favicon report the outcome, so a run left in another tab does not have to
// be reopened to know how it went.
func TestRunPageTitleCarriesStatus(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	for _, tc := range []struct{ id, status, mark string }{
		{"t-ok", "succeeded", "✓"},
		{"t-bad", "failed", "✗"},
		{"t-live", "running", "◐"},
	} {
		err := pipeline.Store.StartRun(ctx, tc.id, "build", "")
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}

		if tc.status != "running" {
			err = pipeline.Store.FinishRun(ctx, tc.id, tc.status)
			if err != nil {
				t.Fatalf("FinishRun: %v", err)
			}
		}

		_, body := get(t, server, "/p/demo/runs/"+tc.id)

		want := "<title>" + tc.mark + " build #" + tc.id + " — steps</title>"
		if !strings.Contains(body, want) {
			t.Errorf("%s: title missing %q", tc.status, want)
		}

		if !strings.Contains(body, `rel="icon"`) {
			t.Errorf("%s: no favicon link", tc.status)
		}
	}
}

// TestStepAnchors pins that every step is addressable by a stable fragment,
// so pointing someone at one is a URL rather than a description.
func TestStepAnchors(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "anchored", "build", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "anchored", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "review[security]", StepKind: "agent"},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "review[security]", StepKind: "agent", Status: "succeeded"},
	})

	_, body := get(t, server, "/p/demo/runs/anchored")

	// An across: cell's brackets must survive into a usable fragment.
	if !strings.Contains(body, `id="step-0-review-security"`) {
		t.Error("step has no anchor id")
	}

	if !strings.Contains(body, `href="#step-0-review-security"`) {
		t.Error("step has no anchor link")
	}
}

// TestFollowRedirectAndPolling covers the trigger-to-run handoff: triggering
// lands on the follow page, which reports no run until one starts and then
// hands over the run's URL.
func TestFollowRedirectAndPolling(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	// A read-only server refuses the trigger, so drive the follow page and
	// its endpoint directly — they are what the redirect targets.
	code, body := get(t, server, "/p/demo/jobs/build/follow?since=0")
	if code != http.StatusOK {
		t.Fatalf("follow page = %d", code)
	}

	if !strings.Contains(body, "forwards to the run") {
		t.Error("follow page does not explain what it is waiting for")
	}

	since := time.Now().UTC().Add(-time.Second).UnixMilli()

	_, empty := get(t, server, fmt.Sprintf("/p/demo/jobs/build/latest-run?since=%d", since))
	if !strings.Contains(empty, `"run":null`) {
		t.Errorf("expected no run yet, got %s", empty)
	}

	err := pipeline.Store.StartRun(ctx, "followed", "build", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	_, found := get(t, server, fmt.Sprintf("/p/demo/jobs/build/latest-run?since=%d", since))
	if !strings.Contains(found, `"run":"followed"`) || !strings.Contains(found, "/p/demo/runs/followed") {
		t.Errorf("expected the started run, got %s", found)
	}

	// A run that started BEFORE the click must not be mistaken for its result.
	later := time.Now().UTC().Add(time.Hour).UnixMilli()

	_, stale := get(t, server, fmt.Sprintf("/p/demo/jobs/build/latest-run?since=%d", later))
	if !strings.Contains(stale, `"run":null`) {
		t.Errorf("a prior run was reported as this trigger's result: %s", stale)
	}

	// A missing or unparseable stamp credits nothing that already exists.
	// Treating it as zero would match the job's whole history and hand back
	// its OLDEST run as though it were the one just triggered.
	for _, query := range []string{"", "?since=", "?since=banana", "?since=0"} {
		_, body := get(t, server, "/p/demo/jobs/build/latest-run"+query)
		if !strings.Contains(body, `"run":null`) {
			t.Errorf("latest-run%q resolved to an existing run: %s", query, body)
		}
	}
}

// TestRelativeTimesAreMachineReadable pins the contract the ticker depends
// on: rendered times carry the absolute instant, so a page left open can keep
// them honest instead of freezing at render time.
func TestRelativeTimesAreMachineReadable(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "ticking", "build", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/jobs/build")
	if !strings.Contains(body, "<time data-ago=") {
		t.Error("run history has no machine-readable timestamps")
	}

	// A running run counts up; a finished one is fixed.
	if !strings.Contains(body, "data-elapsed-since=") {
		t.Error("a running run does not carry its start instant for the ticker")
	}

	err = pipeline.Store.FinishRun(ctx, "ticking", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, done := get(t, server, "/p/demo/jobs/build")
	if strings.Contains(done, "data-elapsed-since=") {
		t.Error("a finished run is still counting up")
	}
}

// TestSlugify covers the anchor-name rules directly, including the shapes
// across: cells and hook labels actually produce.
func TestSlugify(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
		{"compile", "compile"},
		{"review[security]", "review-security"},
		{"Deploy To Prod", "deploy-to-prod"},
		{"unit-tests", "unit-tests"},
		{"a//b", "a-b"},
		{"...", ""},
		{"", ""},
	} {
		if got := slugify(tc.in); got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTranscriptShowsTaskOutput covers the question a reader asks right after
// "did it pass": what did it print. A succeeding step used to expand onto
// nothing.
func TestTranscriptShowsTaskOutput(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "noisy", "build", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "noisy", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "compile", StepKind: "task"},
		{Type: events.TypeStepOutput, StepIndex: 0, StepName: "compile", StepKind: "task", Text: "compiling 42 files"},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "compile", StepKind: "task", Status: "succeeded", DurationMS: 30},
		{Type: events.TypeStepStarted, StepIndex: 1, StepName: "silent", StepKind: "task"},
		{Type: events.TypeStepFinished, StepIndex: 1, StepName: "silent", StepKind: "task", Status: "succeeded", DurationMS: 5},
	})

	err = pipeline.Store.FinishRun(ctx, "noisy", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/noisy")

	if !strings.Contains(body, "compiling 42 files") {
		t.Error("transcript does not show what the task printed")
	}

	// A step with output is expandable; one without is not, so a chevron never
	// promises detail that is not there.
	if !strings.Contains(openingTag(t, body, "step-0-compile"), "data-toggle") {
		t.Error("a step with output is not expandable")
	}

	if strings.Contains(openingTag(t, body, "step-1-silent"), "data-toggle") {
		t.Error("a step with no output is expandable onto nothing")
	}
}

// openingTag returns just the opening tag of the element with the given id.
// Scoped deliberately: a looser search runs on into the page's own scripts,
// which mention the very attributes being asserted on.
func openingTag(t *testing.T, body, id string) string {
	t.Helper()

	start := strings.Index(body, `id="`+id+`"`)
	if start < 0 {
		t.Fatalf("no element with id %q on the page", id)
	}

	end := strings.Index(body[start:], ">")
	if end < 0 {
		t.Fatalf("element %q has no closing bracket", id)
	}

	return body[start : start+end]
}

// TestRunTranscriptHighlightsToolJSON is the readability contract for the
// surface an agent step spends most of its rows on. A tool result arrives as
// JSON whose interesting value is itself a JSON document escaped inside a
// string — the read_file shape — and the page must show that document as a
// document: keys colored, source order kept, and the escaping gone.
func TestRunTranscriptHighlightsToolJSON(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-json", "build", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-json", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "review", StepKind: "agent"},
		{Type: events.TypeAgentCall, StepIndex: 0, StepName: "review", Name: "read_file", Detail: `{"path":"notes/inventory.json"}`},
		{Type: events.TypeAgentResult, StepIndex: 0, StepName: "review", Name: "read_file", Detail: `{"content":"{\"warehouse\":\"sea-1\",\"on_hand\":12,\"ok\":true}"}`},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "review", StepKind: "agent", Status: "succeeded", DurationMS: 120},
	})

	err = pipeline.Store.FinishRun(ctx, "run-json", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	code, body := get(t, server, "/p/demo/runs/run-json")
	if code != http.StatusOK {
		t.Fatalf("GET run = %d", code)
	}

	checks := []struct {
		what string
		// present is markup the page must carry; absent is markup that proves
		// the payload was printed rather than parsed.
		present []string
		absent  []string
	}{
		{
			// Each token carries its own class, so the stylesheet — not the
			// markup — owns what a JSON key looks like.
			what:    "highlighted tokens",
			present: []string{`class="j-key"`, `class="j-str"`, `class="j-num"`, `class="j-lit"`},
		},
		{
			// The document escaped inside "content" is shown unescaped, and its
			// keys are highlighted like any other — only possible if it was
			// parsed rather than printed as a string.
			what:    "embedded document parsed out of its string",
			present: []string{`<span class="j-key">&#34;warehouse&#34;</span>`},
			absent:  []string{`\&#34;warehouse\&#34;`, `\"warehouse\"`},
		},
		{
			// A short args map stays on its row; a bulky result folds behind a
			// disclosure naming what is inside it.
			what:    "bulk folded, small inline",
			present: []string{`<details class="jsonbox"`, "notes/inventory.json"},
		},
	}

	for _, check := range checks {
		for _, want := range check.present {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %s", check.what, want)
			}
		}

		for _, unwanted := range check.absent {
			if strings.Contains(body, unwanted) {
				t.Errorf("%s: still carries %s", check.what, unwanted)
			}
		}
	}

	// Source order, not Go-map order: on_hand precedes ok in the document.
	if strings.Index(body, "on_hand") > strings.Index(body, `j-key">&#34;ok&#34;`) {
		t.Error("JSON keys were re-sorted rather than kept in source order")
	}
}

// TestLiveViewResumesAfterWhatItRendered pins the contract that keeps a
// mid-run page from showing everything twice: the transcript carries the
// highest event sequence the server already drew, and the stream resumes after
// it. Opened at 0 instead, the stream replays events already on the page and
// appends a second copy of every turn.
func TestLiveViewResumesAfterWhatItRendered(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-live", "build", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-live", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "review", StepKind: "agent"},
		{Type: events.TypeAgentCall, StepIndex: 0, StepName: "review", Name: "read_file", Detail: `{"path":"main.go"}`},
		{Type: events.TypeStepOutput, StepIndex: 0, StepName: "review", Text: "compiled 3 packages"},
	})

	// Left running deliberately: the live script only ships for a run in
	// flight, which is the only case this bug could occur in.
	rows, err := pipeline.Store.RunEvents(ctx, "run-live", 0, 5000)
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}

	want := fmt.Sprintf(`data-last-seq="%d"`, rows[len(rows)-1].Seq)

	_, body := get(t, server, "/p/demo/runs/run-live")
	if !strings.Contains(body, want) {
		t.Errorf("transcript does not carry the sequence it rendered (want %s)", want)
	}

	// And the script must read it rather than starting from zero.
	if !strings.Contains(body, "Number(transcript.dataset.lastSeq)") {
		t.Error("live stream does not resume from the rendered sequence")
	}
}

// TestEveryOutputEventSurvives covers a step with attempts:, which publishes
// one output per attempt. The live stream appends a block per event, so holding
// them as a single overwritten string meant the run's closing reload silently
// deleted the attempts a reader had just been watching.
func TestEveryOutputEventSurvives(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-retry", "build", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-retry", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "compile", StepKind: "task"},
		{Type: events.TypeStepOutput, StepIndex: 0, StepName: "compile", Text: "attempt one failed"},
		{Type: events.TypeStepOutput, StepIndex: 0, StepName: "compile", Text: "attempt two failed"},
		{Type: events.TypeStepOutput, StepIndex: 0, StepName: "compile", Text: "attempt three passed"},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "compile", StepKind: "task", Status: "succeeded"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-retry", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-retry")

	for _, want := range []string{"attempt one failed", "attempt two failed", "attempt three passed"} {
		if !strings.Contains(body, want) {
			t.Errorf("transcript dropped an output event: %q missing", want)
		}
	}
}

// TestAnswerIsNotPrintedTwice covers the dedup between the last model text and
// the labeled answer — including the shape that used to slip through, where the
// model emitted text AND a tool call in one message, so the text is not the
// trailing turn.
func TestAnswerIsNotPrintedTwice(t *testing.T) {
	t.Parallel()

	const answer = "Two SKUs need restocking."

	step := stepView{
		Result: map[string]any{"response": answer},
		Turns: []turnView{
			{Type: events.TypeAgentText, Text: "Reading the inventory first."},
			{Type: events.TypeAgentText, Text: answer},
			// Recorded after the answer: the same assistant message carried a
			// tool call, so the result lands last.
			{Type: events.TypeAgentCall, Name: "read_file"},
			{Type: events.TypeAgentResult, Name: "read_file"},
		},
	}

	kept := step.Conversation()
	if len(kept) != 3 {
		t.Fatalf("Conversation kept %d turns, want 3: %+v", len(kept), kept)
	}

	for _, turn := range kept {
		if turn.Text == answer {
			t.Error("the answer is still in the conversation as well as under `answer`")
		}
	}

	// The mid-conversation commentary is not the answer and must survive.
	if kept[0].Text != "Reading the inventory first." {
		t.Errorf("dropped the wrong turn: %+v", kept)
	}

	// A response the model never said as text leaves every turn alone.
	other := stepView{Result: map[string]any{"response": "different"}, Turns: step.Turns}
	if len(other.Conversation()) != len(step.Turns) {
		t.Error("a non-matching response dropped a turn anyway")
	}
}

// TestNodePageOmitsAnAbsentResult pins the section guard that moved when the
// emptiness check moved into jsonPre: a node with nothing recorded must not
// render a "Result" heading over an empty box.
func TestNodePageOmitsAnAbsentResult(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	record := store.NodeRecord{
		Hash:      "aaaa111122223333",
		Kind:      "task",
		StepIndex: 0,
		Resource:  "compile",
		Content:   map[string]any{"run": "true"},
	}

	err := pipeline.Store.RecordNode(ctx, record, "build", "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode: %v", err)
	}

	code, body := get(t, server, "/p/demo/nodes/"+record.Hash)
	if code != http.StatusOK {
		t.Fatalf("GET node = %d: %s", code, body)
	}

	// The content map is the page's point and must render highlighted.
	if !strings.Contains(body, `class="j-key"`) {
		t.Error("node content map is not highlighted")
	}

	if strings.Contains(body, ">Result<") {
		t.Error("node page shows a Result section for a node that recorded none")
	}
}

// TestFailedRunNamesWhatChangedSinceTheLastGreen crosses the seam a failed
// page opens with: attachDiff finds the last green run, reads its steps, and
// diffAgainst names the ones whose content hash moved.
//
// The prior run is read through the narrow path rather than the full page
// assembler — a diff wants step names and hashes and discards the nodes,
// spend and machines assembleRun also fetches — so this is what proves the
// narrow path still carries both halves of what the diff compares.
func TestFailedRunNamesWhatChangedSinceTheLastGreen(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	steps := func(compileHash string) []store.RunEventRow {
		return []store.RunEventRow{
			{Type: events.TypeStepStarted, StepIndex: 0, StepName: "repo", StepKind: "get", StepID: 1},
			{Type: events.TypeStepFinished, StepIndex: 0, StepName: "repo", StepKind: "get", StepID: 1,
				Status: "succeeded", Hash: strings.Repeat("a", 16)},
			{Type: events.TypeStepStarted, StepIndex: 1, StepName: "compile", StepKind: "task", StepID: 2},
			{Type: events.TypeStepFinished, StepIndex: 1, StepName: "compile", StepKind: "task", StepID: 2,
				Status: "failed", Hash: compileHash},
		}
	}

	for _, run := range []struct {
		id, status, hash string
	}{
		{"green", "succeeded", strings.Repeat("b", 16)},
		{"red", "failed", strings.Repeat("c", 16)},
	} {
		err := pipeline.Store.StartRun(ctx, run.id, "build", "")
		if err != nil {
			t.Fatalf("StartRun %s: %v", run.id, err)
		}

		appendEvents(t, pipeline.Store, run.id, steps(run.hash))

		err = pipeline.Store.FinishRun(ctx, run.id, run.status)
		if err != nil {
			t.Fatalf("FinishRun %s: %v", run.id, err)
		}
	}

	code, body := get(t, server, "/p/demo/runs/red")
	if code != http.StatusOK {
		t.Fatalf("GET run = %d: %s", code, body)
	}

	if !strings.Contains(body, `class="chg">compile</span>`) {
		t.Errorf("the failed run does not name the step whose content moved:\n%s", body)
	}

	// The step both runs share is what makes the note worth reading: naming
	// everything is the same as naming nothing.
	if strings.Contains(body, `class="chg">repo</span>`) {
		t.Error("the diff names a step whose hash did not move")
	}

	if !strings.Contains(body, "/p/demo/runs/green") {
		t.Error("the diff note does not link the green run it compared against")
	}
}
