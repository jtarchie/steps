package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/engine"
	"github.com/jtarchie/steps/journal"
	"github.com/jtarchie/steps/machine"
	"github.com/jtarchie/steps/provider"
	"github.com/jtarchie/steps/toolreg"
)

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// parkedRun starts summarize-critic against a never-approving mock so it
// parks at the escalate gate, and returns a server plus the parked run id.
func parkedRun(t *testing.T) (*server, string, *engine.Engine, *machine.Machine) {
	t.Helper()
	t.Chdir(t.TempDir())

	root := repoRoot()
	store, err := journal.OpenSQLite(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	eng := engine.New(store, provider.NewRegistry(), toolreg.New(), engine.NopListener{})
	mockPath := filepath.Join(t.TempDir(), "never.yaml")
	err = os.WriteFile(mockPath, []byte(neverApprovesScript), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := provider.LoadScript(mockPath)
	if err != nil {
		t.Fatal(err)
	}
	eng.Mock = loaded

	m, err := machine.Load(filepath.Join(root, "examples/summarize-critic/workflow.ts"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.Start(context.Background(), m, map[string]any{"article": "a short article about containers"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != journal.StatusParked {
		t.Fatalf("status = %s, want parked", res.Status)
	}
	return &server{store: store, eng: eng}, res.RunID, eng, m
}

const neverApprovesScript = `
draft:
  - text: '{"summary": "Draft one.", "key_points": ["a", "b", "c"]}'
  - text: '{"summary": "Draft two.", "key_points": ["a", "b", "c"]}'
  - text: '{"summary": "Draft three.", "key_points": ["a", "b", "c"]}'
critique:
  - text: '{"score": 3, "issues": ["too short"], "event": "revise"}'
  - text: '{"score": 4, "issues": ["still short"], "event": "revise"}'
  - text: '{"score": 5, "issues": ["nope"], "event": "revise"}'
`

func TestServeRunsList(t *testing.T) {
	s, id, _, _ := parkedRun(t)
	e := newServer(t.Context(), s.store, s.eng, nil, 0)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/runs", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /runs = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{id, "summarize-critic", "parked", "Needs attention"} {
		if !strings.Contains(body, want) {
			t.Errorf("/runs body missing %q", want)
		}
	}
}

func TestServeRunDetailShowsGateForm(t *testing.T) {
	s, id, _, _ := parkedRun(t)
	e := newServer(t.Context(), s.store, s.eng, nil, 0)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/runs/"+id, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /runs/%s = %d, want 200", id, rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`name="event" value="approved"`,
		`name="event" value="rejected"`,
		"Ship the current draft as-is",
		`name="note"`,
		"Waiting on you",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run detail missing %q\n", want)
		}
	}
}

func TestServeRunDetailTimeline(t *testing.T) {
	s, id, _, _ := parkedRun(t)
	e := newServer(t.Context(), s.store, s.eng, nil, 0)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/runs/"+id, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /runs/%s = %d, want 200", id, rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Timeline",                         // the merged routing+conversations section
		`→ <code>critique`,                 // a transition connector in the timeline
		"ollama/qwen3:8b",                  // per-step model label (draft), from the pinned machine
		"a short article about containers", // the run's inputs, shown in the Inputs block
		"Draft one.",                       // journaled step output content is readable
		"parked at",                        // the gate park marker
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run detail timeline missing %q", want)
		}
	}
}

func TestServeRunDetailGraph(t *testing.T) {
	s, id, _, _ := parkedRun(t)
	e := newServer(t.Context(), s.store, s.eng, nil, 0)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/runs/"+id, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /runs/%s = %d, want 200", id, rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<h2>Machine</h2>",     // the diagram section renders
		"<svg",                 // as inline SVG
		"graph-svg has-run",    // with this run overlaid
		`data-name="escalate"`, // the gate state is a node
		`data-name="critique"`, // the judge state is a node
		" current",             // the parked gate is marked current
		" parked",              // and parked
		" fired",               // at least one traversed edge is emphasized
		"drag to pan",          // the pan/zoom affordance is wired
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run detail graph missing %q", want)
		}
	}
}

func TestServeDoneRunArtifacts(t *testing.T) {
	s, id, eng, m := parkedRun(t)
	// Approve the gate directly to completion, then render the finished run.
	_, err := eng.Resume(context.Background(), m, id, "approved", nil)
	if err != nil {
		t.Fatal(err)
	}
	run, err := eng.Store.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != journal.StatusDone {
		t.Fatalf("status = %s, want done after approving", run.Status)
	}

	e := newServer(t.Context(), s.store, s.eng, nil, 0)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/runs/"+id, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		"Artifacts",
		"out/summary.md", // the written file, from its {path, bytes} output
		" B",             // a journaled byte size (no disk read)
		"wrote <code>",   // the write step notes what it produced in the timeline
	} {
		if !strings.Contains(body, want) {
			t.Errorf("done-run detail missing %q", want)
		}
	}
}

func TestServeResumeAdvancesRun(t *testing.T) {
	s, id, eng, _ := parkedRun(t)
	e := newServer(t.Context(), s.store, s.eng, nil, 0)

	form := url.Values{"event": {"approved"}, "note": {"ship it"}}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/runs/"+id+"/resume", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST resume = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/runs/"+id {
		t.Errorf("redirect = %q, want /runs/%s", loc, id)
	}

	// Resume runs in a background goroutine — poll the run row to done.
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := eng.Store.GetRun(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == journal.StatusDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run status = %s, want done within 5s", run.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestServeResumeRejectsNotParked(t *testing.T) {
	s, id, eng, m := parkedRun(t)
	// Consume the park directly so the run is no longer parked.
	_, err := eng.Resume(context.Background(), m, id, "rejected", nil)
	if err != nil {
		t.Fatal(err)
	}
	e := newServer(t.Context(), s.store, s.eng, nil, 0)

	form := url.Values{"event": {"approved"}}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/runs/"+id+"/resume", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("POST resume on finished run = %d, want 409", rec.Code)
	}
}

func TestHookStartsRun(t *testing.T) {
	t.Chdir(t.TempDir())

	wf := `
const work = {
  write: "out/incident.txt",
  content: ({ incident }) => incident,
};
export default {
  name: "hooked",
  model: "mock",
  input: { incident: { type: "string", required: true }, region: { type: "string", required: true } },
  states: { work },
  webhook: { path: "hb", map: ({ body, region }) => ({ incident: body.fault.klass + " in " + region }) },
};`
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}

	store, err := journal.OpenSQLite(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	eng := engine.New(store, provider.NewRegistry(), toolreg.New(), engine.NopListener{})

	hook := &hookSpec{m: m, inputs: map[string]any{"region": "us-east-1"}, token: "sekrit"}
	e := newServer(t.Context(), store, eng, map[string]*hookSpec{m.Webhook.Path: hook}, 1)

	post := func(target, jsonBody string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, target, strings.NewReader(jsonBody))
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	// wrong/missing token -> 401
	if rec := post("/hooks/hb", `{"fault": {"klass": "Redis::TimeoutError"}}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", rec.Code)
	}
	// unknown path -> 404
	if rec := post("/hooks/nope?token=sekrit", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown hook = %d, want 404", rec.Code)
	}
	// malformed JSON -> 400
	if rec := post("/hooks/hb?token=sekrit", `{nope`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON = %d, want 400", rec.Code)
	}

	// good payload -> 202, run completes in background, artifact written
	if rec := post("/hooks/hb?token=sekrit", `{"fault": {"klass": "Redis::TimeoutError"}}`); rec.Code != http.StatusAccepted {
		t.Fatalf("hook = %d body=%s, want 202", rec.Code, rec.Body.String())
	}

	waitForSingleRun(t, store, journal.StatusDone)
	raw, err := os.ReadFile("out/incident.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "Redis::TimeoutError in us-east-1" {
		t.Errorf("incident = %q — payload body and hook-input should both reach the map", raw)
	}
}

func TestHookMissingRequiredInput(t *testing.T) {
	t.Chdir(t.TempDir())

	// severity is required but the map never produces it -> 400.
	wf := `
const work = { write: "out/x.txt", content: ({ incident }) => incident };
export default {
  name: "hooked",
  model: "mock",
  input: { incident: { type: "string", required: true }, severity: { type: "string", required: true } },
  states: { work },
  webhook: { path: "hb", map: ({ body }) => ({ incident: body.message }) },
};`
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}

	store, err := journal.OpenSQLite(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	eng := engine.New(store, provider.NewRegistry(), toolreg.New(), engine.NopListener{})
	hook := &hookSpec{m: m, inputs: map[string]any{}}
	e := newServer(t.Context(), store, eng, map[string]*hookSpec{m.Webhook.Path: hook}, 1)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/hooks/hb",
		strings.NewReader(`{"message": "boom"}`))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing required = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "severity") {
		t.Errorf("400 body = %q, should name the missing input", rec.Body.String())
	}
}

// TestWebhookTriggersIncidentRunbook drives the full serve lifecycle: a
// Honeybadger-shaped webhook starts the run, it parks at the multi gate, and
// the gate is answered through the existing web form.
func TestWebhookTriggersIncidentRunbook(t *testing.T) {
	t.Chdir(t.TempDir())

	root := repoRoot()
	wf := filepath.Join(root, "examples", "incident-runbook", "workflow.ts")
	m, err := machine.Load(wf, machine.WithEngineDefaultModel("mock/scripted"))
	if err != nil {
		t.Fatal(err)
	}

	store, err := journal.OpenSQLite(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	eng := engine.New(store, provider.NewRegistry(), toolreg.New(), engine.NopListener{})
	script, err := provider.LoadScript(filepath.Join(root, "examples", "incident-runbook", "mock_responses.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	eng.Mock = script

	// Real fixture server for the probes; dead endpoint for the tracker.
	docroot := filepath.Join(root, "examples", "incident-runbook", "fixtures", "serve")
	fixtures := httptest.NewServer(http.FileServer(http.Dir(docroot)))
	t.Cleanup(fixtures.Close)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()

	hook := &hookSpec{
		m: m,
		inputs: map[string]any{
			"services":    "api,worker,cache,search",
			"status_base": fixtures.URL + "/status",
			"hb_base":     dead.URL, // tracker down -> the escalation path
		},
		token: "sekrit",
	}
	e := newServer(t.Context(), store, eng, map[string]*hookSpec{m.Webhook.Path: hook}, 1)

	payload, err := os.ReadFile(filepath.Join(root, "examples", "incident-runbook", "fixtures", "webhook.json"))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/hooks/honeybadger?token=sekrit", strings.NewReader(string(payload)))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("hook = %d body=%s, want 202", rec.Code, rec.Body.String())
	}

	// The run starts in the background: poll until it parks at the gate.
	runID := waitForSingleRun(t, store, journal.StatusParked)
	checkMappedIncidentInputs(t, store, runID)

	// Answer the multi gate through the existing web form.
	form := url.Values{
		"selected": {"Restart the cache cluster", "Enable request coalescing on api"},
		"note":     {"hold the scale-up"},
	}
	req = httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/runs/"+runID+"/resume", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("resume = %d, want 303", rec.Code)
	}

	waitForRunStatus(t, store, runID, journal.StatusDone)

	report, err := os.ReadFile("out/incident-report.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), "hold the scale-up") {
		t.Error("gate note should reach the report")
	}
}

// waitForSingleRun polls until exactly one run reaches the wanted status and
// returns its id.
func waitForSingleRun(t *testing.T, store journal.Store, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		runs, err := store.ListRuns(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 && runs[0].Status == want {
			return runs[0].ID
		}
		if time.Now().After(deadline) {
			t.Fatalf("no run reached %s in time; runs=%+v", want, runs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForRunStatus polls one run until it reaches the wanted status.
func waitForRunStatus(t *testing.T, store journal.Store, runID, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		run, err := store.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run status = %s, want %s", run.Status, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestParseHookTokens(t *testing.T) {
	tokens, fallback := parseHookTokens([]string{"honeybadger=s1", "github=s2", "bare-fallback"})
	if tokens["honeybadger"] != "s1" || tokens["github"] != "s2" {
		t.Errorf("per-path tokens = %v, want honeybadger=s1 github=s2", tokens)
	}
	if fallback != "bare-fallback" {
		t.Errorf("fallback = %q, want bare-fallback", fallback)
	}
	// A secret may itself contain '='; only the first '=' splits.
	tokens, _ = parseHookTokens([]string{"hb=a=b=c"})
	if tokens["hb"] != "a=b=c" {
		t.Errorf("token with '=' = %q, want a=b=c", tokens["hb"])
	}
}

// writeHook builds a mock write-action machine keyed by webhook path.
func writeHook(t *testing.T, name, path, outFile string, token string) *hookSpec {
	t.Helper()
	wf := `
const work = { write: "` + outFile + `", content: ({ msg }) => msg };
export default {
  name: "` + name + `",
  model: "mock",
  input: { msg: { type: "string", required: true } },
  states: { work },
  webhook: { path: "` + path + `", map: ({ body }) => ({ msg: "` + name + `:" + body.v }) },
};`
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	return &hookSpec{m: m, inputs: map[string]any{}, token: token}
}

// TestHookMultiRouting registers two hooks and asserts POSTs route to the right
// machine by path, with per-hook token enforcement.
func TestHookMultiRouting(t *testing.T) {
	t.Chdir(t.TempDir())
	store, err := journal.OpenSQLite(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	eng := engine.New(store, provider.NewRegistry(), toolreg.New(), engine.NopListener{})

	hookA := writeHook(t, "hooked-a", "a", "out/a.txt", "tok-a")
	hookB := writeHook(t, "hooked-b", "b", "out/b.txt", "") // b is unauthenticated
	hooks := map[string]*hookSpec{"a": hookA, "b": hookB}
	e := newServer(t.Context(), store, eng, hooks, 2)

	post := func(target, body string) int {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, target, strings.NewReader(body))
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post("/hooks/a", `{"v": "1"}`); code != http.StatusUnauthorized {
		t.Fatalf("hook a without token = %d, want 401", code)
	}
	if code := post("/hooks/zzz?token=whatever", `{"v": "1"}`); code != http.StatusNotFound {
		t.Fatalf("unknown path = %d, want 404", code)
	}
	if code := post("/hooks/a?token=tok-a", `{"v": "1"}`); code != http.StatusAccepted {
		t.Fatalf("hook a = %d, want 202", code)
	}
	if code := post("/hooks/b", `{"v": "2"}`); code != http.StatusAccepted {
		t.Fatalf("hook b (no token) = %d, want 202", code)
	}

	waitForNRuns(t, store, journal.StatusDone, 2)
	byMachine := map[string]bool{}
	runs, _ := store.ListRuns(context.Background())
	for _, r := range runs {
		byMachine[r.Machine] = true
	}
	if !byMachine["hooked-a"] || !byMachine["hooked-b"] {
		t.Errorf("machines = %v, want both hooked-a and hooked-b", byMachine)
	}
	a, _ := os.ReadFile("out/a.txt")
	b, _ := os.ReadFile("out/b.txt")
	if string(a) != "hooked-a:1" || string(b) != "hooked-b:2" {
		t.Errorf("artifacts a=%q b=%q, want payloads routed to their own machine", a, b)
	}
}

// TestHookQueueFull429 fills a hook's durable queue (no dispatcher draining) and
// asserts the overflowing POST is rejected with 429 — isolating the admission
// arithmetic from dispatch timing.
func TestHookQueueFull429(t *testing.T) {
	t.Chdir(t.TempDir())
	wf := `
const work = { write: "out/x.txt", content: ({ msg }) => msg };
export default {
  name: "capped",
  model: "mock",
  input: { msg: { type: "string", required: true } },
  states: { work },
  webhook: { path: "hb", map: ({ body }) => ({ msg: body.v }), maxQueued: 2 },
};`
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	store, err := journal.OpenSQLite(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	eng := engine.New(store, provider.NewRegistry(), toolreg.New(), engine.NopListener{})

	// Bare server: no dispatcher, so the queue never drains between POSTs.
	s := &server{store: store, eng: eng, hooksByPath: map[string]*hookSpec{"hb": {m: m, inputs: map[string]any{}}}}
	e := echo.New()
	e.POST("/hooks/:path", s.handleHook)

	post := func() int {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/hooks/hb", strings.NewReader(`{"v": "x"}`))
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post(); code != http.StatusAccepted { // queue -> 1
		t.Fatalf("post 1 = %d, want 202", code)
	}
	if code := post(); code != http.StatusAccepted { // queue -> 2 (== maxQueued)
		t.Fatalf("post 2 = %d, want 202", code)
	}
	if code := post(); code != http.StatusTooManyRequests { // full -> 429
		t.Fatalf("post 3 = %d, want 429", code)
	}
}

// TestHookDurableQueueDrain proves queued runs survive a "restart": rows are
// enqueued with no server running, then a fresh newServer's startup scan drains
// them to completion.
func TestHookDurableQueueDrain(t *testing.T) {
	t.Chdir(t.TempDir())
	store, err := journal.OpenSQLite(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	eng := engine.New(store, provider.NewRegistry(), toolreg.New(), engine.NopListener{})

	hook := writeHook(t, "recover", "cr", "out/cr.txt", "")

	// Pre-crash: three durably-queued runs, no dispatcher yet.
	for i := range 3 {
		_, err := eng.Enqueue(context.Background(), hook.m, map[string]any{"msg": "run" + string(rune('0'+i))}, "cr")
		if err != nil {
			t.Fatal(err)
		}
	}
	queued, _ := store.ListRunsByStatus(context.Background(), journal.StatusQueued)
	if len(queued) != 3 {
		t.Fatalf("queued = %d, want 3 before restart", len(queued))
	}

	// "Restart": a fresh server over the same journal. Its dispatcher's first
	// drain (global cap 1 = strictly serial) picks up all queued rows.
	newServer(t.Context(), store, eng, map[string]*hookSpec{"cr": hook}, 1)

	waitForNRuns(t, store, journal.StatusDone, 3)
	left, _ := store.ListRunsByStatus(context.Background(), journal.StatusQueued)
	if len(left) != 0 {
		t.Errorf("still queued = %d, want 0 after drain", len(left))
	}
}

// waitForNRuns polls until at least n runs reach the wanted status.
func waitForNRuns(t *testing.T, store journal.Store, want string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		runs, err := store.ListRunsByStatus(context.Background(), want)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d runs reached %s, want %d", len(runs), want, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func checkMappedIncidentInputs(t *testing.T, store journal.Store, runID string) {
	t.Helper()
	events, err := store.Events(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	rs := journal.Fold(events)
	if incident, _ := rs.Ctx["incident"].(string); !strings.Contains(incident, "83214792") || !strings.Contains(incident, "Redis::TimeoutError") {
		t.Errorf("mapped incident = %q, want payload-derived text", rs.Ctx["incident"])
	}
	if faultURL, _ := rs.Ctx["fault_url"].(string); !strings.Contains(faultURL, "/v2/projects/8412/faults/83214792") {
		t.Errorf("mapped fault_url = %q, want composed from hb_base + payload ids", rs.Ctx["fault_url"])
	}
}
