package web

// The live view.
//
// A running job streams its events to the page as they happen. The stream is
// deliberately built on the same rows the finished-run page renders from: a
// client says which sequence number it already has, gets everything after it,
// and keeps getting more. That means a reader who opens a run mid-flight and
// a reader who opens it an hour later are looking at the same thing, and a
// dropped connection costs nothing but a reconnect.
//
// What travels is HTML, rendered by the same `step` template the page
// renders, and htmx's hx-sse extension swaps it in. It used to be JSON, with
// a second renderer in the browser rebuilding every row, turn and JSON
// payload from it — around 700 lines that had to agree with model.go,
// jsonview.go and the step template, and repeatedly did not: the toggle
// affordance, the open/container classes, the agent's answer, the "stopped
// early" badge and the worker name each shipped drawn by the server and not
// by the stream, and each one alone made a reader reload. One renderer is the
// fix; there is now nothing for the two to disagree about.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/jtarchie/steps/internal/store"
)

// livePollInterval is how often a streaming connection re-reads the store.
//
// The store is polled rather than the bus subscribed to, because the bus only
// carries runs THIS process is executing: a run started by a separate
// `steps run` in another terminal writes to the same database and would be
// invisible to a subscriber. Polling one indexed table by sequence number is
// cheap, and it is the only approach that shows every run rather than a
// privileged subset.
const livePollInterval = 400 * time.Millisecond

// liveIdleTimeout ends a stream that has gone quiet on a run that is over,
// so a forgotten browser tab does not hold a connection forever.
const liveIdleTimeout = 5 * time.Minute

// liveBatch is how many events one read of the stream takes. The stream pages
// rather than reading a run whole, which is what keeps it working past
// runEventLimit — the page's own bound, and one a delta has no reason to
// inherit.
const liveBatch = 500

// handleRunEvents streams a run's events as server-sent events.
func (s *Server) handleRunEvents(c echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()
	runID := c.Param("run")

	after := resumeFrom(c)
	// What the reader is looking at: everything at or before the sequence
	// they resumed from, plus every row sent since on THIS connection. It is
	// what decides whether a fragment is morphed onto a row or appended as a
	// new one, and getting it wrong is silent either way.
	drawn := drawnAt(after)
	// The fold stays open for the life of the connection. Re-reading the run
	// on every tick was the obvious way to render a delta and the wrong one
	// twice over: it re-folded thousands of events 2.5 times a second per
	// watcher, and it stopped at runEventLimit — past that the flush had no
	// touched step in its truncated view, so it wrote nothing at all while
	// holding the socket open and re-arming the idle deadline. A reader
	// watched a frozen transcript on a live connection, with nothing logged.
	folder := newRunFolder()

	// Catch the fold up to what the reader is already looking at, without
	// sending any of it: their page was rendered from exactly these events,
	// and the fold has to hold them or a row they can see is missing from
	// every fragment that follows — a container whose children arrived before
	// they connected would re-render with none of them.
	err := s.seedFold(c, runID, after, folder)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	response := openStream(c)

	ticker := time.NewTicker(livePollInterval)
	defer ticker.Stop()

	deadline := time.NewTimer(liveIdleTimeout)
	defer deadline.Stop()

	for {
		// Read the run BEFORE flushing, not after: the flush needs the row
		// anyway, and a run that finishes mid-flush is still seen as running
		// here, so the loop comes round once more and delivers its last events
		// before saying done.
		run, ok, err := pipeline.Store.FindRunRow(ctx, runID)
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}

		if !ok {
			writeSSE(response, "done", map[string]any{"status": run.Status})

			return nil
		}

		before := after

		after, err = s.flushEvents(c, run, after, drawn, folder)
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}

		// Activity re-arms the deadline: it bounds SILENCE, not the run. Armed
		// once and never reset, it would cut the stream on every job longer
		// than the timeout — which is the case a live view exists for.
		if after != before {
			deadline.Reset(liveIdleTimeout)
		}

		// Once the run is over and its events are all delivered, say so and
		// close: the page has everything, and holding the socket open would
		// only poll a table that can no longer change.
		if run.Status != "running" {
			writeSSE(response, "done", map[string]any{"status": run.Status})

			return nil
		}

		if !waitForMore(ctx, ticker.C, deadline.C, response) {
			return nil
		}
	}
}

// waitForMore holds until the next poll, reporting whether the stream should
// keep going. Silence long enough to trip the deadline ends it: the run is
// still running, so this says idle rather than claiming an outcome.
func waitForMore(ctx context.Context, tick, deadline <-chan time.Time, response *echo.Response) bool {
	select {
	case <-ctx.Done():
		return false
	case <-deadline:
		writeSSE(response, "done", map[string]any{"status": "idle"})

		return false
	case <-tick:
		return true
	}
}

// openStream puts the response into server-sent-event mode.
func openStream(c echo.Context) *echo.Response {
	response := c.Response()
	response.Header().Set(echo.HeaderContentType, "text/event-stream")
	response.Header().Set("Cache-Control", "no-cache")
	response.Header().Set("Connection", "keep-alive")
	// Without this an intermediary that buffers by default (a proxy someone
	// put in front of this) turns a live stream into one big delivery at the
	// end, which is the exact opposite of the feature.
	response.Header().Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)

	return response
}

// seedFold folds everything up to seq into the fold, in pages, so a run
// longer than one read stays whole. Nothing is written to the client: this is
// the history they already have on the page.
func (s *Server) seedFold(c echo.Context, runID string, seq int64, folder *runFolder) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	for at := int64(0); at < seq; {
		rows, err := pipeline.Store.RunEvents(ctx, runID, at, liveBatch)
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}

		if len(rows) == 0 {
			return nil
		}

		// Anything past the reader's own sequence belongs to the first flush,
		// which has to SEND it.
		for len(rows) > 0 && rows[len(rows)-1].Seq > seq {
			rows = rows[:len(rows)-1]
		}

		if len(rows) == 0 {
			return nil
		}

		nodes, err := pipeline.Store.NodesByHash(ctx, hashesOf(rows))
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}

		folder.add(rows, nodes)

		at = rows[len(rows)-1].Seq
	}

	return nil
}

// resumeFrom is the sequence number this connection already has.
//
// Last-Event-ID first: it is what the browser resends on a reconnect, and it
// describes what that client actually applied. The query parameter is the
// FIRST connection's answer, stamped into the page by the server that
// rendered the transcript — without it a reader who opens a run mid-flight
// would be sent every event again and see each row twice.
func resumeFrom(c echo.Context) int64 {
	if id := c.Request().Header.Get("Last-Event-ID"); id != "" {
		seq, err := strconv.ParseInt(id, 10, 64)
		if err == nil {
			return seq
		}
	}

	seq, _ := strconv.ParseInt(c.QueryParam("after"), 10, 64)

	return seq
}

// flushEvents writes what changed since seq and returns the new high-water
// mark.
//
// The unit is a ROOT step, not an event: a step's row shows facts no single
// event carries — a container's rollup changes when its third child finishes,
// and nothing publishes an event on the container — so the fragment is
// re-rendered from the assembled view, which is the same one the page
// renders. Only the roots the flushed events touched are sent, which is what
// keeps a page-sized payload off the wire for a one-line change.
func (s *Server) flushEvents(
	c echo.Context, run store.RunRow, after int64, drawn func(*stepView) bool, folder *runFolder,
) (int64, error) {
	// Drained, not read once: a tick reads a bounded batch, and a run that
	// recorded more than one batch between ticks — or that had already
	// finished when the reader connected — would otherwise deliver a batch
	// and close, leaving a transcript that ends mid-run with no sign it was
	// cut. Each batch is its own message, so what a reconnect resumes from
	// stays exact.
	for {
		before := after

		next, err := s.flushBatch(c, run, after, drawn, folder)
		if err != nil {
			return next, err
		}

		after = next

		if after == before {
			return after, nil
		}
	}
}

func (s *Server) flushBatch(
	c echo.Context, run store.RunRow, after int64, drawn func(*stepView) bool, folder *runFolder,
) (int64, error) {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	rows, err := pipeline.Store.RunEvents(ctx, run.ID, after, liveBatch)
	if err != nil {
		return after, fmt.Errorf("web: %w", err)
	}

	if len(rows) == 0 {
		return after, nil
	}

	touched, after := reach(rows, after)

	// Only the nodes THIS batch names: a finished agent step's answer and
	// trajectory live in its node, and the rest of the run's nodes are
	// already folded in.
	nodes, err := pipeline.Store.NodesByHash(ctx, hashesOf(rows))
	if err != nil {
		return after, fmt.Errorf("web: %w", err)
	}

	folder.add(rows, nodes)

	view := folder.view(run)

	// The `step` template reads one thing off the nav — the slug its node
	// links are scoped by — and s.nav() would buy that with the two pending
	// counts nothing in a row displays, 2.5 times a second per watcher.
	page := map[string]any{"Nav": navData{Current: pipeline.Slug}, "Run": view}

	// ONE message per flush, however many rows changed. The id a browser
	// resends on a reconnect names the last message it applied, so a flush
	// split across several messages could be resumed from the middle of
	// itself: the rows in the frames that never arrived would be skipped, and
	// an appended row skipped that way is gone for the rest of the run —
	// every later flush renders it as a swap onto an id the page never drew.
	// Whole flush or nothing.
	var frame strings.Builder

	for _, root := range view.Roots {
		if !subtreeTouched(root, touched) {
			continue
		}

		// A row the reader has is morphed onto it; one they do not have is
		// appended, because there is nothing there to morph — and htmx drops
		// an out-of-band swap at a missing id without a word.
		fragment, err := s.renderStep(page, root, drawn(root))
		if err != nil {
			return after, err
		}

		frame.WriteString(fragment)
	}

	if frame.Len() == 0 {
		return after, nil
	}

	writeFrame(c.Response(), after, frame.String())

	c.Response().Flush()

	return after, nil
}

// reach reads what a flush is ABOUT: the steps its events name, and the
// sequence it ends at.
func reach(rows []store.RunEventRow, after int64) (touched map[string]bool, seq int64) {
	touched = map[string]bool{}

	for _, row := range rows {
		touched[stepKey(row)] = true
		after = row.Seq
	}

	return touched, after
}

// drawnAt answers, for one connection, whether the reader already has a row.
//
// A row is theirs if the event that put it on the page came at or before the
// sequence they resumed from — the page they are looking at was rendered from
// exactly those events — or if this connection has since sent it. Asking
// instead whether a step.started arrived in the current flush was almost the
// same question and wrong for the case that has no start at all: a step
// swallowed by a chain skip is opened by its step.skipped, so its row was
// swapped over an id nothing had drawn and vanished.
func drawnAt(origin int64) func(*stepView) bool {
	sent := map[string]bool{}

	return func(step *stepView) bool {
		has := step.FirstSeq <= origin || sent[step.Key()]
		sent[step.Key()] = true

		return has
	}
}

// subtreeTouched reports whether any step in this root's subtree was named by
// the flushed events.
func subtreeTouched(step *stepView, touched map[string]bool) bool {
	if touched[step.Key()] {
		return true
	}

	for _, child := range step.Children {
		if subtreeTouched(child, touched) {
			return true
		}
	}

	return false
}

// renderStep renders one step's subtree with the page's own `step` template.
// oob asks for the attribute that swaps it over the row already on the page;
// without it the fragment is wrapped to be appended to the transcript.
func (s *Server) renderStep(page map[string]any, step *stepView, oob bool) (string, error) {
	tmpl, ok := s.renderer.pages["run"]
	if !ok {
		return "", fmt.Errorf("web: no run template to render %q with", step.Name)
	}

	var out bytes.Buffer

	err := tmpl.ExecuteTemplate(&out, "step", stepCtx{Page: page, Step: step, OOB: oob})
	if err != nil {
		return "", fmt.Errorf("web: could not render step %q: %w", step.Name, err)
	}

	if oob {
		return out.String(), nil
	}

	// beforeend strips this wrapper and appends what is inside it, which is
	// how a row that does not exist yet gets onto the page at all.
	return `<div hx-swap-oob="beforeend:#transcript">` + out.String() + `</div>`, nil
}

// frameLines cuts a fragment into the data lines one SSE message carries.
//
// html/template escapes the five HTML metacharacters and NOT the carriage
// return, but SSE ends a line on CRLF, CR *or* LF — so a lone CR in a step's
// output (every progress bar writes them) ends the data line early and the
// markup after it is read as SSE fields. `\r\revent: done\rdata: 0\r\r` on a
// task's stdout is a forged run-completion frame: it closes the reader's
// stream and paints the tab red on a job that is still running. A CRLF is a
// line ending like any other; a lone CR is content, and goes over as the
// entity so the frame's shape stays the server's decision.
func frameLines(html string) []string {
	normalized := strings.ReplaceAll(html, "\r\n", "\n")

	return strings.Split(strings.ReplaceAll(normalized, "\r", "&#13;"), "\n")
}

// writeFrame emits one HTML fragment as an unnamed SSE message, which is the
// shape hx-sse swaps. Named messages dispatch a DOM event instead, which is
// what `done` is for.
func writeFrame(response *echo.Response, id int64, html string) {
	_, _ = fmt.Fprintf(response, "id: %d\n", id)

	// One data: line per line of markup. SSE rejoins them with newlines, so a
	// <pre> block arrives with its whitespace intact — the whole point of
	// sending markup the server already escaped.
	for _, line := range frameLines(html) {
		_, _ = fmt.Fprintf(response, "data: %s\n", line)
	}

	_, _ = fmt.Fprint(response, "\n")
	response.Flush()
}

// writeSSE emits one named event carrying JSON. A marshal failure is skipped
// rather than killing the stream: one unrenderable event must not end the
// run's live view.
func writeSSE(response *echo.Response, name string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	_, _ = fmt.Fprintf(response, "event: %s\ndata: %s\n\n", name, data)
	response.Flush()
}
