package web

// A tool result's body is fetched when its box is opened, not drawn with the
// page (#171): results are nearly all of a long agent run's page, and nearly
// none of them are ever opened.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// bulkyResult is a read_file result too tall to sit on a row, so the page
// folds it and fetches its body on open.
const bulkyResult = `{"content":"package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"}`

var resultSrc = regexp.MustCompile(`hx-get="(/p/[^"]+/turns/\d+)"`)

// resultSrcs is every fragment URL a page or stream points a result box at.
func resultSrcs(body string) []string {
	matches := resultSrc.FindAllStringSubmatch(body, -1)
	srcs := make([]string, 0, len(matches))

	for _, match := range matches {
		srcs = append(srcs, match[1])
	}

	return srcs
}

// getHX is get as htmx asks: the fragment route answers it with the <pre>'s
// inner HTML rather than a page.
func getHX(t *testing.T, server *Server, target string) (int, string) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	req.Header.Set("HX-Request", "true")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Code, rec.Body.String()
}

// seqsOf is every event of a run, by type, in order.
func seqsOf(t *testing.T, st store.Store, runID string) map[string][]int64 {
	t.Helper()

	rows, err := st.RunEvents(t.Context(), runID, 0, 1000)
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}

	seqs := map[string][]int64{}
	for _, row := range rows {
		seqs[row.Type] = append(seqs[row.Type], row.Seq)
	}

	return seqs
}

// TestTurnFragmentIsWhatThePageUsedToDraw: opening a box shows exactly what
// the page drew inline before bodies were fetched — for a document and for
// plain text, which takes the other highlighting path.
func TestTurnFragmentIsWhatThePageUsedToDraw(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	plain := strings.Repeat("a line of a command's output\n", 40)

	err := pipeline.Store.StartRun(ctx, "run-frag", "build", "", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-frag", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "review", StepKind: "agent"},
		{Type: events.TypeAgentResult, StepIndex: 0, StepName: "review", Name: "read_file", Detail: bulkyResult},
		{Type: events.TypeAgentResult, StepIndex: 0, StepName: "review", Name: "run", Detail: plain},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "review", StepKind: "agent", Status: "succeeded"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-frag", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, page := get(t, server, "/p/demo/runs/run-frag")

	srcs := resultSrcs(page)
	if len(srcs) != 2 {
		t.Fatalf("page carries %d result URLs, want 2: %v", len(srcs), srcs)
	}

	for i, detail := range []string{bulkyResult, plain} {
		want := string(jsonValue(detail).HTML)

		code, body := getHX(t, server, srcs[i])
		if code != http.StatusOK || body != want {
			t.Errorf("GET %s = %d, not the body the page used to draw.\ngot:\n%s\nwant:\n%s", srcs[i], code, body, want)
		}

		// With JavaScript off the placeholder is a link, and it has to land
		// on something readable: whitespace kept, spans styled.
		code, body = get(t, server, srcs[i])
		if code != http.StatusOK || !strings.Contains(body, `<pre class="json">`+want+`</pre>`) || !strings.Contains(body, `href="/static/app.css"`) {
			t.Errorf("GET %s without htmx = %d, not a styled page around the body:\n%s", srcs[i], code, body)
		}
	}
}

// TestTurnFragmentAnswersOnlyForAResultOfThisRun: the route serves a result
// the page drew lazily and nothing else, and says so as one line of text — a
// failed fetch lands inside the box, where handleError's page would nest the
// nav and its scripts in a <pre>.
func TestTurnFragmentAnswersOnlyForAResultOfThisRun(t *testing.T) {
	t.Parallel()

	server, pipelines := testPipelines(t, "alpha", "beta")
	alpha, beta := pipelines[0], pipelines[1]
	ctx := t.Context()

	for _, run := range []struct {
		pipeline *Pipeline
		id       string
	}{{alpha, "run-a1"}, {alpha, "run-a2"}, {beta, "run-b1"}} {
		err := run.pipeline.Store.StartRun(ctx, run.id, run.pipeline.Slug+"-job", "", "")
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}

		rows := []store.RunEventRow{
			{Type: events.TypeStepStarted, StepIndex: 0, StepName: "review", StepKind: "agent"},
			{Type: events.TypeAgentCall, StepIndex: 0, StepName: "review", Name: "read_file", Detail: `{"path":"main.go"}`},
			{Type: events.TypeAgentResult, StepIndex: 0, StepName: "review", Name: "read_file", Detail: bulkyResult},
		}

		// run-a2's first row is a result, so an earlier run's seq asked of
		// it finds a result of the WRONG run as its next row.
		if run.id == "run-a2" {
			rows = rows[2:]
		}

		appendEvents(t, run.pipeline.Store, run.id, rows)
	}

	a1, a2, b1 := seqsOf(t, alpha.Store, "run-a1"), seqsOf(t, alpha.Store, "run-a2"), seqsOf(t, beta.Store, "run-b1")
	result := a1[events.TypeAgentResult][0]

	// The control: without it every case below passes against a route that
	// answers nothing at all.
	code, _ := getHX(t, server, fmt.Sprintf("/p/alpha/runs/run-a1/turns/%d", result))
	if code != http.StatusOK {
		t.Fatalf("a result of this run = %d, want 200", code)
	}

	for _, tc := range []struct{ what, path string }{
		{"a seq from another run", fmt.Sprintf("/p/alpha/runs/run-a1/turns/%d", a2[events.TypeAgentResult][0])},
		{"a seq from an earlier run", fmt.Sprintf("/p/alpha/runs/run-a2/turns/%d", result)},
		{"a run from another pipeline", fmt.Sprintf("/p/alpha/runs/run-b1/turns/%d", b1[events.TypeAgentResult][0])},
		{"a seq that is not a result", fmt.Sprintf("/p/alpha/runs/run-a1/turns/%d", a1[events.TypeAgentCall][0])},
		{"a seq past the end", fmt.Sprintf("/p/alpha/runs/run-a1/turns/%d", b1[events.TypeAgentResult][0]+100)},
		{"not a number", "/p/alpha/runs/run-a1/turns/abc"},
		{"zero", "/p/alpha/runs/run-a1/turns/0"},
		{"negative", "/p/alpha/runs/run-a1/turns/-1"},
		{"past int64", "/p/alpha/runs/run-a1/turns/99999999999999999999"},
		{"an unknown pipeline", fmt.Sprintf("/p/nope/runs/run-a1/turns/%d", result)},
	} {
		code, body := getHX(t, server, tc.path)
		if code != http.StatusNotFound {
			t.Errorf("%s: GET %s = %d, want 404", tc.what, tc.path, code)
		}

		for _, page := range []string{"<html", "<script", "statusline"} {
			if strings.Contains(body, page) {
				t.Errorf("%s: the 404 is a page (%s), not a line for the box:\n%s", tc.what, page, body)
			}
		}
	}
}

// TestStreamedResultArrivesAsASummary: a result that reaches a watcher over
// the stream is folded the same way, and its URL serves its body.
func TestStreamedResultArrivesAsASummary(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-streamed", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-streamed", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "review", StepKind: "agent", StepID: 1},
		{Type: events.TypeAgentText, StepIndex: 0, StepName: "review", StepID: 1, Text: "Reading."},
	})

	_, page := get(t, server, "/p/demo/runs/run-streamed")
	after := regexp.MustCompile(`/events\?after=(\d+)"`).FindStringSubmatch(page)

	if after == nil {
		t.Fatal("the page does not say what sequence it was drawn from")
	}

	const marker = "STREAMED-RESULT-MARKER"

	appendEvents(t, pipeline.Store, "run-streamed", []store.RunEventRow{
		{Type: events.TypeAgentResult, StepIndex: 0, StepName: "review", StepID: 1, Name: "read_file", Detail: `{"content":"` + marker + `\nsecond line"}`},
	})

	err = pipeline.Store.FinishRun(ctx, "run-streamed", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	stream := sseHTML(streamOf(t, server, "/p/demo/runs/run-streamed/events?after="+after[1]))
	if strings.Contains(stream, marker) {
		t.Errorf("the stream carries the result's body:\n%s", stream)
	}

	srcs := resultSrcs(stream)
	if len(srcs) != 1 {
		t.Fatalf("the stream carries %d result URLs, want 1:\n%s", len(srcs), stream)
	}

	_, body := getHX(t, server, srcs[0])
	if !strings.Contains(body, marker) {
		t.Errorf("GET %s does not serve the streamed result:\n%s", srcs[0], body)
	}
}

// TestResultURLEscapesThePipelineName: hx-get is a plain attribute to
// html/template, which HTML-escapes it and never URL-encodes it, so a `#` in
// a pipeline's name cut the fetch short at /p/<prefix> — another route's page,
// swapped into the box.
func TestResultURLEscapesThePipelineName(t *testing.T) {
	t.Parallel()

	server, pipelines := testPipelines(t, "rev#2")
	pipeline := pipelines[0]

	err := pipeline.Store.StartRun(t.Context(), "run-escaped", "rev#2-job", "", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-escaped", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "review", StepKind: "agent"},
		{Type: events.TypeAgentResult, StepIndex: 0, StepName: "review", Name: "read_file", Detail: bulkyResult},
	})

	_, page := get(t, server, "/p/rev%232/runs/run-escaped")

	srcs := resultSrcs(page)
	if len(srcs) != 1 {
		t.Fatalf("page carries %d result URLs, want 1: %v", len(srcs), srcs)
	}

	// A browser splits the URL before it asks; httptest does not.
	src, err := url.Parse(srcs[0])
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", srcs[0], err)
	}

	if src.Fragment != "" || src.RawQuery != "" {
		t.Fatalf("a browser reads %q as path %q", srcs[0], src.Path)
	}

	code, body := getHX(t, server, src.EscapedPath())
	if code != http.StatusOK || body != string(jsonValue(bulkyResult).HTML) {
		t.Errorf("GET %s = %d, not the result's body:\n%s", srcs[0], code, body)
	}
}

// TestTurnFragmentVariesOnHTMX: one URL answers htmx with a fragment and a
// followed link with a page, so a cache that is not told so can hand either
// one the other's bytes.
func TestTurnFragmentVariesOnHTMX(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	err := pipeline.Store.StartRun(t.Context(), "run-vary", "build", "", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-vary", []store.RunEventRow{
		{Type: events.TypeAgentResult, StepIndex: 0, StepName: "review", Name: "read_file", Detail: bulkyResult},
	})

	seq := seqsOf(t, pipeline.Store, "run-vary")[events.TypeAgentResult][0]
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/p/demo/runs/run-vary/turns/%d", seq), nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	vary := strings.Join(rec.Header().Values("Vary"), ", ")
	if rec.Code != http.StatusOK || !strings.Contains(vary, "HX-Request") {
		t.Errorf("GET = %d, Vary %q: want 200 varying on HX-Request", rec.Code, vary)
	}
}

// TestTurnFragmentServesTheFirstEvent: seq 1 is a real event, and the guard
// against seq-1 underflowing must not refuse it.
func TestTurnFragmentServesTheFirstEvent(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	err := pipeline.Store.StartRun(t.Context(), "run-first", "build", "", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-first", []store.RunEventRow{
		{Type: events.TypeAgentResult, StepIndex: 0, StepName: "review", Name: "read_file", Detail: bulkyResult},
	})

	if seq := seqsOf(t, pipeline.Store, "run-first")[events.TypeAgentResult][0]; seq != 1 {
		t.Fatalf("the result is seq %d; this test needs the database's first event", seq)
	}

	code, body := getHX(t, server, "/p/demo/runs/run-first/turns/1")
	if code != http.StatusOK || body != string(jsonValue(bulkyResult).HTML) {
		t.Errorf("GET /turns/1 = %d, want the first event's body:\n%s", code, body)
	}
}
