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
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/jtarchie/steps/internal/events"
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

// handleRunEvents streams a run's events as server-sent events.
func (s *Server) handleRunEvents(c echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()
	runID := c.Param("run")

	after := resumeFrom(c)

	response := c.Response()
	response.Header().Set(echo.HeaderContentType, "text/event-stream")
	response.Header().Set("Cache-Control", "no-cache")
	response.Header().Set("Connection", "keep-alive")
	// Without this an intermediary that buffers by default (a proxy someone
	// put in front of this) turns a live stream into one big delivery at the
	// end, which is the exact opposite of the feature.
	response.Header().Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)

	ticker := time.NewTicker(livePollInterval)
	defer ticker.Stop()

	deadline := time.NewTimer(liveIdleTimeout)
	defer deadline.Stop()

	for {
		var err error

		before := after

		after, err = s.flushEvents(c, runID, after)
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
		run, ok, err := pipeline.Store.FindRunRow(ctx, runID)
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}

		if !ok || run.Status != "running" {
			writeSSE(response, "done", map[string]any{"status": run.Status})

			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			writeSSE(response, "done", map[string]any{"status": "idle"})

			return nil
		case <-ticker.C:
		}
	}
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
func (s *Server) flushEvents(c echo.Context, runID string, after int64) (int64, error) {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	rows, err := pipeline.Store.RunEvents(ctx, runID, after, 500)
	if err != nil {
		return after, fmt.Errorf("web: %w", err)
	}

	if len(rows) == 0 {
		return after, nil
	}

	touched, opened, after := reach(rows, after)

	run, ok, err := pipeline.Store.FindRunRow(ctx, runID)
	if err != nil {
		return after, fmt.Errorf("web: %w", err)
	}

	if !ok {
		return after, nil
	}

	view, err := s.assembleRun(c, run)
	if err != nil {
		return after, fmt.Errorf("web: %w", err)
	}

	page := map[string]any{"Nav": s.nav(c), "Run": view}

	for _, root := range view.Roots {
		if !subtreeTouched(root, touched) {
			continue
		}

		// A root the client has is morphed onto the row already on the page;
		// one this flush OPENED is appended, because there is nothing there to
		// morph. The client's own drawing is exactly the events at or before
		// `after`, so a root whose step.started arrives in this flush is a row
		// it cannot have — including on the first connection, where the page
		// itself drew everything up to the sequence it asked to resume from.
		fragment, err := s.renderStep(page, root, !opened[root.Key()])
		if err != nil {
			return after, err
		}

		// Every frame of one flush carries the SAME id, deliberately. A
		// connection that dies between two frames leaves the client resuming
		// past the whole flush: it loses the frames it never got (the closing
		// reload draws them) rather than replaying the ones it applied, which
		// on an appended row would draw it twice.
		writeFrame(c.Response(), after, fragment)
	}

	c.Response().Flush()

	return after, nil
}

// reach reads what a flush is ABOUT: the steps its events name, the ones it
// opened, and the sequence it ends at.
func reach(rows []store.RunEventRow, after int64) (touched, opened map[string]bool, seq int64) {
	touched = map[string]bool{}
	opened = map[string]bool{}

	for _, row := range rows {
		key := stepKey(row)
		touched[key] = true

		if row.Type == events.TypeStepStarted {
			opened[key] = true
		}

		after = row.Seq
	}

	return touched, opened, after
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

// writeFrame emits one HTML fragment as an unnamed SSE message, which is the
// shape hx-sse swaps. Named messages dispatch a DOM event instead, which is
// what `done` is for.
func writeFrame(response *echo.Response, id int64, html string) {
	_, _ = fmt.Fprintf(response, "id: %d\n", id)

	// One data: line per line of markup. SSE rejoins them with newlines, so a
	// <pre> block arrives with its whitespace intact — the whole point of
	// sending markup the server already escaped.
	for _, line := range strings.Split(html, "\n") {
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
