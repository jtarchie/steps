package web

// The live stream ships the page's own markup.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// streamOf opens the run's event stream and returns the raw SSE body. The run
// must already be finished, or the stream never closes.
func streamOf(t *testing.T, server *Server, path string) string {
	t.Helper()

	body := make(chan string, 1)

	go func() {
		// The test's context, not Background: on the t.Fatal below this
		// goroutine is abandoned, and under Background it keeps polling for
		// the whole liveIdleTimeout — reading liveBatch past the t.Cleanup
		// that restores it, which is a data race on the failure path of the
		// very test that shrinks it.
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		body <- rec.Body.String()
	}()

	select {
	case out := <-body:
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("SSE stream did not close for a finished run")

		return ""
	}
}

// sseHTML rejoins the markup an SSE body carries, which is how a browser
// reads it: every data: line of a message, joined by newlines.
func sseHTML(body string) string {
	var out []string

	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			out = append(out, strings.TrimPrefix(line, "data: "))
		}
	}

	return strings.Join(out, "\n")
}

// TestStreamDrawsTheSameMarkupThePageDoes is this package's standing rule,
// made structural. It used to be enforced one field at a time — the toggle
// affordance, the open class, the agent's answer, the "stopped early" badge,
// the worker — because a second renderer in the browser rebuilt every row
// from JSON and had to be taught each of them separately. Each was found by a
// reader who had to reload.
//
// Now the stream renders with the page's own template, so the assertion can
// be the whole thing: every row the page draws must appear in the stream,
// byte for byte, differing only by the attribute that swaps it into place.
func TestStreamDrawsTheSameMarkupThePageDoes(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-same", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// A shape with something of everything the row can carry: a cached step,
	// an agent with a conversation and an answer, and a container whose child
	// failed — the rollup no single event describes.
	appendEvents(t, pipeline.Store, "run-same", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "repo", StepKind: "get", StepID: 1},
		{Type: events.TypeStepSkipped, StepIndex: 0, StepName: "repo", StepKind: "get", StepID: 1, Status: "skipped", Hash: "cafe1234567", Text: "unchanged — replayed from cache"},
		{Type: events.TypeStepStarted, StepIndex: 1, StepName: "review", StepKind: "agent", StepID: 2},
		{Type: events.TypeAgentText, StepIndex: 1, StepName: "review", StepID: 2, Text: "Reading the diff first."},
		{Type: events.TypeAgentCall, StepIndex: 1, StepName: "review", StepID: 2, Name: "read_file", Detail: `{"path":"main.go"}`},
		{Type: events.TypeStepFinished, StepIndex: 1, StepName: "review", StepKind: "agent", StepID: 2, Status: "succeeded", Hash: "beef7654321", DurationMS: 4200},
		{Type: events.TypeStepStarted, StepIndex: 2, StepName: "matrix", StepKind: "across", StepID: 3},
		{Type: events.TypeStepStarted, StepIndex: 3, StepName: "cell", StepKind: "task", StepID: 4, ParentStepID: 3},
		{Type: events.TypeStepFinished, StepIndex: 3, StepName: "cell", StepKind: "task", StepID: 4, ParentStepID: 3, Status: "failed", Text: "exit 1", Worker: "gpu (ssh://jt@box)"},
		{Type: events.TypeStepFinished, StepIndex: 2, StepName: "matrix", StepKind: "across", StepID: 3, Status: "failed"},
	})

	mustRecordResult(t, pipeline, "beef7654321", map[string]any{"response": "Looks fine.", "wrapped_up": true})

	err = pipeline.Store.FinishRun(ctx, "run-same", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, page := get(t, server, "/p/demo/runs/run-same")
	// after=0: every row is new to this connection, so the stream draws all
	// of them — which is exactly the comparison worth making.
	stream := sseHTML(streamOf(t, server, "/p/demo/runs/run-same/events"))

	for _, anchor := range []string{"step-1-repo", "step-2-review", "step-3-matrix"} {
		drawn := elementAt(t, page, `id="`+anchor+`"`)

		// The one difference between the two renderings is the attribute that
		// puts the row where it goes.
		streamed := strings.ReplaceAll(stream, ` hx-swap-oob="outerMorph"`, "")

		if !strings.Contains(streamed, drawn) {
			t.Errorf("the stream does not draw #%s the way the page does.\npage:\n%s\n\nstream:\n%s",
				anchor, drawn, stream)
		}
	}

	// And the run's own facts ride along inside those rows, rather than being
	// separately taught to a second renderer.
	for _, want := range []string{
		"unchanged — replayed from cache",
		"Reading the diff first.",
		"read_file",
		"Looks fine.",
		"stopped early",
		"on gpu (ssh://jt@box)",
		"1 failed",
	} {
		if !strings.Contains(stream, want) {
			t.Errorf("the stream never draws %q", want)
		}
	}
}

// elementAt returns the element whose opening tag carries mark, from its "<"
// through its closing tag.
func elementAt(t *testing.T, body, mark string) string {
	t.Helper()

	at := strings.Index(body, mark)
	if at < 0 {
		t.Fatalf("no element carrying %s", mark)
	}

	open := strings.LastIndex(body[:at], "<")

	name := body[open+1:]
	if end := strings.IndexAny(name, " \t\r\n>"); end >= 0 {
		name = name[:end]
	}

	return body[open:closeOf(t, body, open, name)]
}

// TestStreamAppendsWhatThePageHasNotDrawnAndMorphsWhatItHas: the two ways a
// fragment can land, and picking the wrong one is visible either way. A row
// the page already drew must be morphed onto that row — appended, it appears
// twice. A row that opened after the page was rendered has nothing to morph
// onto, and an out-of-band swap at a missing id is dropped on the floor, so
// the step would never appear at all.
func TestStreamAppendsWhatThePageHasNotDrawnAndMorphsWhatItHas(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-split", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-split", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "early", StepKind: "task", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "early", StepKind: "task", StepID: 1, Status: "succeeded"},
		{Type: events.TypeStepStarted, StepIndex: 1, StepName: "late", StepKind: "task", StepID: 2},
		{Type: events.TypeStepFinished, StepIndex: 1, StepName: "late", StepKind: "task", StepID: 2, Status: "succeeded"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-split", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	// A reader whose page was rendered after `early` started but before it
	// finished: `early` is a row on their page, `late` is not.
	stream := sseHTML(streamOf(t, server, "/p/demo/runs/run-split/events?after=1"))

	early := strings.Index(stream, `id="step-1-early"`)
	late := strings.Index(stream, `id="step-2-late"`)

	if early < 0 || late < 0 {
		t.Fatalf("stream drew early at %d and late at %d — one is missing:\n%s", early, late, stream)
	}

	if !strings.Contains(stream[:late], `hx-swap-oob="outerMorph"`) {
		t.Errorf("the row the page already drew is not morphed onto it:\n%s", stream)
	}

	if !strings.Contains(stream[:late], `hx-swap-oob="beforeend:#transcript"`) {
		t.Errorf("the row that opened after the page was drawn is not appended to the transcript:\n%s", stream)
	}
}

// TestStreamResumesFromLastEventID: the header a browser resends on a
// reconnect is what says where to pick up. Without it a dropped connection
// replays the run from the sequence baked into the page's URL, and every row
// recorded since arrives a second time.
func TestStreamResumesFromLastEventID(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-resume", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-resume", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "first", StepKind: "task", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "first", StepKind: "task", StepID: 1, Status: "succeeded"},
		{Type: events.TypeStepStarted, StepIndex: 1, StepName: "second", StepKind: "task", StepID: 2},
		{Type: events.TypeStepFinished, StepIndex: 1, StepName: "second", StepKind: "task", StepID: 2, Status: "succeeded"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-resume", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	body := make(chan string, 1)

	go func() {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
			"/p/demo/runs/run-resume/events?after=0", nil)
		// The browser's answer wins over the URL's: the URL is what the page
		// was rendered with, the header is what this client actually applied.
		req.Header.Set("Last-Event-ID", "2")
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		body <- rec.Body.String()
	}()

	var raw string

	select {
	case raw = <-body:
	case <-time.After(5 * time.Second):
		t.Fatal("SSE stream did not close for a finished run")
	}

	if strings.Contains(sseHTML(raw), `id="step-1-first"`) {
		t.Errorf("resuming from Last-Event-ID redrew a row the client already had:\n%s", raw)
	}

	if !strings.Contains(sseHTML(raw), `id="step-2-second"`) {
		t.Errorf("resuming from Last-Event-ID skipped what came after it:\n%s", raw)
	}

	// Every frame is stamped, or the browser has nothing to resume WITH.
	if !strings.HasPrefix(raw, "id: ") {
		t.Errorf("the stream stamps no event ids:\n%s", raw)
	}
}

// TestStreamFramesSurviveACarriageReturn: the wire is line-oriented and the
// payload is command output, which is the one combination html/template does
// not cover. It escapes the five HTML metacharacters and not CR — but an SSE
// reader ends a line on CRLF, CR *or* LF, so a bare CR in a step's output
// ends the data line early and everything after it is read as SSE FIELDS.
// A task that prints `\r\revent: done\r\r` forges the run-completion frame:
// the reader's stream closes and the tab goes red on a job still running.
func TestStreamFramesSurviveACarriageReturn(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-cr", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-cr", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "pull", StepKind: "task", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "pull", StepKind: "task", StepID: 1,
			Status: "failed", Text: "50%\r\revent: done\rdata: {\"status\":\"failed\"}\r\r100%"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-cr", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	raw := streamOf(t, server, "/p/demo/runs/run-cr/events")

	// Read it the way the vendored extension does: split on all three
	// terminators, and dispatch a message on every blank line that ends one.
	var messages, named, fields int

	for _, line := range regexp.MustCompile(`\r\n|\r|\n`).Split(raw, -1) {
		if line == "" {
			if fields > 0 {
				messages++
			}

			fields = 0

			continue
		}

		fields++

		if strings.HasPrefix(line, "event: ") {
			named++
		}
	}

	// One content frame plus the closing `done`, and `done` is the only named
	// event on the wire — the step's own output must not have minted a second.
	if named != 1 {
		t.Errorf("step output forged %d named SSE events, want only `done`:\n%q", named, raw)
	}

	if messages != 2 {
		t.Errorf("the stream carried %d messages, want the frame and its done:\n%q", messages, raw)
	}
}

// TestStreamAppendsIntoTheContainerThePageDraws is the seam: the server picks
// the id it appends into and the template picks the id it draws, and nothing
// tied the two together — renaming the container left every test green while
// every step that started after page load vanished, because htmx drops an
// out-of-band swap whose target is not there.
func TestStreamAppendsIntoTheContainerThePageDraws(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-target", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	_, page := get(t, server, "/p/demo/runs/run-target")

	// The run page must ask for the stream at all, with the extension the
	// layout serves — this is the only place that wiring is asserted.
	for _, want := range []string{`hx-sse:connect="/p/demo/runs/run-target/events?after=`,
		`hx-sse:close="done"`, `hx-on:done="runFinished(event)"`} {
		if !strings.Contains(page, want) {
			t.Errorf("a running run's page does not carry %s", want)
		}
	}

	appendEvents(t, pipeline.Store, "run-target", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "compile", StepKind: "task", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "compile", StepKind: "task", StepID: 1,
			Status: "succeeded"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-target", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	stream := sseHTML(streamOf(t, server, "/p/demo/runs/run-target/events"))

	target := regexp.MustCompile(`hx-swap-oob="beforeend:#([\w-]+)"`).FindStringSubmatch(stream)
	if target == nil {
		t.Fatalf("the stream appends nothing, so a step that opens after the page loads never lands:\n%s", stream)
	}

	if !strings.Contains(page, `id="`+target[1]+`"`) {
		t.Errorf("the stream appends into #%s, which the page never draws — htmx drops the swap", target[1])
	}
}

// TestEmptyTranscriptPlaceholderIsNotAStepToWalk: the placeholder exists for
// the stream's sake — it is always in the DOM so `beforeend:#transcript` has
// somewhere to append beside, and CSS hides it once a real row lands. That
// makes it a `.step` on every run page, and the keyboard walk in layout.html
// selects `.step`, so an unfiltered walk parks the reader on an invisible row
// in the MIDDLE of the list (the stream appends after the placeholder).
func TestEmptyTranscriptPlaceholderIsNotAStepToWalk(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-placeholder", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-placeholder", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "compile", StepKind: "task", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "compile", StepKind: "task", StepID: 1,
			Status: "succeeded"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-placeholder", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, page := get(t, server, "/p/demo/runs/run-placeholder")

	if !strings.Contains(page, `<div class="step" id="run-empty">`) {
		t.Fatal("the placeholder is no longer a .step, so the walk needs no exception — drop it")
	}

	if !strings.Contains(page, `querySelectorAll('.step:not(#run-empty)')`) {
		t.Error("the step walk collects the hidden placeholder, so j/k lands on a row nobody can see")
	}
}

// TestStreamAppendsAChainSkippedRow: a step swallowed by a chain skip never
// starts — its ONLY event is step.skipped, and the view opens the row from
// that. Asking "did a start arrive in this flush" therefore answered no for a
// row the reader had never been sent, so it was swapped over an id nothing
// had drawn and htmx dropped it without a word: the transcript simply stopped
// at the last executed step.
func TestStreamAppendsAChainSkippedRow(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-chain", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-chain", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "compile", StepKind: "task", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "compile", StepKind: "task", StepID: 1, Status: "succeeded"},
		// No start: the chain skip swallowed it.
		{Type: events.TypeStepSkipped, StepIndex: 1, StepName: "ship", StepKind: "put", StepID: 2,
			Status: "skipped", Text: "unchanged — replayed from cache"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-chain", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	// A reader whose page was drawn before the skip was recorded.
	stream := sseHTML(streamOf(t, server, "/p/demo/runs/run-chain/events?after=2"))

	at := strings.Index(stream, `id="step-2-ship"`)
	if at < 0 {
		t.Fatalf("the stream never draws the chain-skipped row:\n%s", stream)
	}

	if !strings.Contains(stream[:at], `hx-swap-oob="beforeend:#transcript"`) {
		t.Errorf("the chain-skipped row is swapped over an id the page never drew:\n%s", stream)
	}
}

// TestOneMessagePerFlush: a browser resends the id of the last message it
// APPLIED, so a flush split across several messages can be resumed from the
// middle of itself — and a row appended in a message that never arrived is
// gone for the rest of the run, since every later flush renders it as a swap
// onto an id the page does not have.
func TestOneMessagePerFlush(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-atomic", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// Two roots, both changed by the same flush.
	appendEvents(t, pipeline.Store, "run-atomic", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "one", StepKind: "task", StepID: 1},
		{Type: events.TypeStepStarted, StepIndex: 1, StepName: "two", StepKind: "task", StepID: 2},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "one", StepKind: "task", StepID: 1, Status: "succeeded"},
		{Type: events.TypeStepFinished, StepIndex: 1, StepName: "two", StepKind: "task", StepID: 2, Status: "succeeded"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-atomic", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	raw := streamOf(t, server, "/p/demo/runs/run-atomic/events")

	if !strings.Contains(sseHTML(raw), `id="step-1-one"`) || !strings.Contains(sseHTML(raw), `id="step-2-two"`) {
		t.Fatalf("the stream did not draw both rows:\n%s", raw)
	}

	// Both rows, one id to resume from. The `done` message is the other one.
	if got := strings.Count(raw, "\nid: "); got != 0 {
		t.Errorf("a flush wrote %d messages after the first, want the whole flush in one:\n%s", got, raw)
	}
}

// TestStreamKeepsDrawingPastTheRunEventLimit: the page reads a run whole and
// stops at runEventLimit, and for a moment the stream inherited that bound by
// re-reading the run on every flush. Past 5,000 events the touched steps were
// simply absent from the truncated view, so the flush wrote NOTHING while the
// connection stayed open and the idle deadline kept re-arming — a frozen
// transcript on a live socket, with nothing logged anywhere. The stream pages
// instead, and folds each page into the view it already has.
func TestStreamKeepsDrawingPastTheRunEventLimit(t *testing.T) {
	shrinkRunEventLimit(t, 40, 10)

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-long", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// One step, then chatter past the page's bound, then the step that has to
	// still be drawn on the other side of it.
	rows := make([]store.RunEventRow, 0, runEventLimit+13)
	rows = append(rows,
		store.RunEventRow{Type: events.TypeStepStarted, StepIndex: 0, StepName: "chatty", StepKind: "agent", StepID: 1})

	for range runEventLimit + 10 {
		rows = append(rows, store.RunEventRow{
			Type: events.TypeAgentText, StepIndex: 0, StepName: "chatty", StepID: 1, Text: "turn",
		})
	}

	rows = append(rows,
		store.RunEventRow{Type: events.TypeStepFinished, StepIndex: 0, StepName: "chatty", StepKind: "agent", StepID: 1, Status: "succeeded"},
		store.RunEventRow{Type: events.TypeStepStarted, StepIndex: 1, StepName: "after-the-bound", StepKind: "task", StepID: 2},
		store.RunEventRow{Type: events.TypeStepFinished, StepIndex: 1, StepName: "after-the-bound", StepKind: "task", StepID: 2, Status: "succeeded"},
	)

	appendEvents(t, pipeline.Store, "run-long", rows)

	err = pipeline.Store.FinishRun(ctx, "run-long", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	stream := sseHTML(streamOf(t, server, "/p/demo/runs/run-long/events"))

	if !strings.Contains(stream, `id="step-2-after-the-bound"`) {
		t.Errorf("the stream stops at the page's event limit, and says nothing about it")
	}
}

// shrinkRunEventLimit lowers the transcript bound for one test, so a test
// about what happens PAST it does not have to write five thousand rows — one
// SQLite transaction each — to get there.
//
// batch moves with it so the run still outruns one read of the stream, which
// is the other thing this fixture proves; see liveBatch.
//
// The test that uses it gives up t.Parallel() in exchange: these are package
// globals, and a parallel sibling reading the page would see the shrunk bounds.
// That is a good trade, because the events were the whole cost: the test was
// the slowest in the package by an order of magnitude and is now among the
// fastest, and a serial test that takes 80ms costs less wall clock than a
// parallel one that takes eight seconds.
func shrinkRunEventLimit(t *testing.T, limit, batch int) {
	t.Helper()

	previousLimit, previousBatch := runEventLimit, liveBatch
	runEventLimit, liveBatch = limit, batch

	t.Cleanup(func() { runEventLimit, liveBatch = previousLimit, previousBatch })
}

// TestStreamCarriesASubAgentsTurns crosses the seam between what the fold
// does with an event and what the flush sends.
//
// A sub-agent's conversation events carry the CHILD's name, not the plan
// step's, so attachTurn hangs them on the agent step still running — a
// deliberate fallback. A flush that decided what to send by asking
// stepKey(row) instead held a key belonging to no row: the fold changed, no
// root matched, and the whole of a sub-agent conversation appeared only at
// the closing reload.
//
// Serial, because shrinkRunEventLimit writes package globals: one event per
// batch is what puts the turn in a flush of its own, which is the case the
// bug needed.
func TestStreamCarriesASubAgentsTurns(t *testing.T) {
	shrinkRunEventLimit(t, 40, 1)

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-sub", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-sub", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1},
		// No step id and a name no step.started used: what a delegated
		// conversation publishes.
		{Type: events.TypeAgentText, StepIndex: 0, StepName: "helper", Text: "the sub-agent said this"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-sub", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	stream := sseHTML(streamOf(t, server, "/p/demo/runs/run-sub/events"))
	if !strings.Contains(stream, "the sub-agent said this") {
		t.Errorf("a sub-agent's turn never reaches the stream:\n%s", stream)
	}
}

// TestStreamRetractsARowItsContainerAdopts: a step is hung under its
// container only once the container has a row, and the two events do not
// arrive in that order — a container swallowed by a chain skip is opened by
// its OWN step.skipped, recorded after the steps inside it. Until then the
// child is a root and the reader carries it at the transcript's top level, so
// appending the container puts a SECOND element with that id on the page.
// htmx resolves every later out-of-band swap to the first match, which leaves
// the orphan updating and the real row frozen — forever, and in silence.
func TestStreamRetractsARowItsContainerAdopts(t *testing.T) {
	shrinkRunEventLimit(t, 40, 1)

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-adopt", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-adopt", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "cell", StepKind: "task", StepID: 2, ParentStepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "cell", StepKind: "task", StepID: 2,
			ParentStepID: 1, Status: "succeeded"},
		{Type: events.TypeStepSkipped, StepIndex: 0, StepName: "matrix", StepKind: "across", StepID: 1,
			Status: "skipped", Text: "cached"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-adopt", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	raw := streamOf(t, server, "/p/demo/runs/run-adopt/events")

	stream := sseHTML(raw)
	if !strings.Contains(stream, `<div id="step-2-cell" hx-swap-oob="delete">`) {
		t.Fatalf("the container's append does not retract the row it adopts:\n%s", stream)
	}

	// Ahead of the append, because htmx applies out-of-band swaps in the
	// order they arrive: after it, the delete would take the copy the append
	// had just drawn.
	retract := strings.Index(stream, `<div id="step-2-cell" hx-swap-oob="delete">`)
	if adopts := strings.Index(stream, `<div id="step-1-matrix"`); adopts >= 0 && adopts < retract {
		t.Errorf("the retraction arrives after the append it exists to precede:\n%s", stream)
	}
}

// TestStreamRefusesAnUnknownRun: openStream commits the response, and echo's
// error handler returns on Committed without rendering or logging — so a run
// this pipeline does not have answered 200 and an event-stream carrying the
// zero row's empty status, which the page reloaded on into a 404 it could not
// explain. The check belongs before the first byte.
func TestStreamRefusesAnUnknownRun(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	code, _ := get(t, server, "/p/demo/runs/no-such-run/events")
	if code != http.StatusNotFound {
		t.Errorf("the stream for a run that does not exist answered %d, want 404", code)
	}
}
