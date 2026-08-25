package web

// The live view.
//
// A running job streams its events to the page as they happen. The stream is
// deliberately built on the same rows the finished-run page renders from: a
// client says which sequence number it already has, gets everything after it,
// and keeps getting more. That means a reader who opens a run mid-flight and
// a reader who opens it an hour later are looking at the same thing, and a
// dropped connection costs nothing but a reconnect.

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
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

	after, _ := strconv.ParseInt(c.QueryParam("after"), 10, 64)

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

// flushEvents writes every event after seq and returns the new high-water
// mark.
func (s *Server) flushEvents(c echo.Context, runID string, after int64) (int64, error) {
	pipeline := pipelineOf(c)

	rows, err := pipeline.Store.RunEvents(c.Request().Context(), runID, after, 500)
	if err != nil {
		return after, fmt.Errorf("web: %w", err)
	}

	for _, row := range rows {
		answer, wrapped := s.finishedAgentStep(c, row)
		writeSSE(c.Response(), "event", liveEvent{RunEventRow: row, Response: answer, WrappedUp: wrapped})
		after = row.Seq
	}

	if len(rows) > 0 {
		c.Response().Flush()
	}

	return after, nil
}

// answerFor is the rendered answer a finishing agent step produced, empty for
// every other event.
//
// The response lives in the step's NODE, not in its event, so a live reader
// saw an agent work for six minutes and then saw the one thing it produced
// only after the run's closing reload. One lookup, on the one event type that
// can have one.
//
// Rendered here rather than in the browser: the answer is markdown, and the
// renderer that makes model-authored markdown safe to put on a page is a Go
// one (prose.go). Its whole contract is that its output is safe HTML — the
// same bytes the server-rendered page ships — so handing those bytes to the
// client is not a second trust decision. Re-implementing a markdown parser in
// the browser to avoid it would be, and would drift.
func (s *Server) finishedAgentStep(c echo.Context, row store.RunEventRow) (answer template.HTML, wrappedUp bool) {
	if row.Type != events.TypeStepFinished || row.StepKind != "agent" || row.Hash == "" {
		return "", false
	}

	node, ok, err := pipelineOf(c).Store.FindNode(c.Request().Context(), row.Hash)
	if err != nil || !ok || node.Result == "" {
		return "", false
	}

	// One decode, every marker the row carries. Pulling only "response" out
	// of this map is what left "stopped early" reload-only: the fact was
	// already in hand and dropped on the floor, so a reader watching a
	// reviewer exhaust its turn budget saw the wrap-up answer arrive looking
	// exactly like a confident one.
	result := decodeResult(node.Result)

	text, _ := result["response"].(string)
	wrapped, _ := result["wrapped_up"].(bool)

	return renderProse(text), wrapped
}

// liveEvent is the wire shape of one event. Deliberately close to the stored
// row: the client renders a live event and a replayed one with the same code.
type liveEvent struct {
	store.RunEventRow
	Response template.HTML
	// WrappedUp mirrors stepView.WrappedUp for the live path. internal/web's
	// standing rule: anything the server draws for a finished step, the
	// stream has to draw too, or a reader who watched and a reader who
	// reloaded see different rows.
	WrappedUp bool
}

// MarshalJSON renders the event with the derived fields a client needs
// (a human-readable duration, the nesting depth already parsed) so the
// browser does no interpretation the server can do once.
func (e liveEvent) MarshalJSON() ([]byte, error) {
	//nolint:wrapcheck // marshaling a fixed struct cannot fail in a way the caller can act on
	return json.Marshal(map[string]any{
		"seq":            e.Seq,
		"type":           e.Type,
		"step_index":     e.StepIndex,
		"step_name":      e.StepName,
		"step_kind":      e.StepKind,
		"step_id":        e.StepID,
		"parent_step_id": e.ParentStepID,
		"response":       string(e.Response),
		"wrapped_up":     e.WrappedUp,
		"status":         e.Status,
		"hash":           e.Hash,
		"text":           e.Text,
		"name":           e.Name,
		"detail":         e.Detail,
		"duration":       formatDuration(time.Duration(e.DurationMS) * time.Millisecond),
		"worker":         e.Worker,
		"depth":          parseDepth(e.Status),
		"agent":          isAgentEvent(e.Type),
	})
}

// isAgentEvent reports conversation traffic, which the client renders as a
// turn rather than as a step transition.
func isAgentEvent(eventType string) bool {
	switch eventType {
	case events.TypeAgentText, events.TypeAgentCall, events.TypeAgentResult, events.TypeAgentSubagent:
		return true
	default:
		return false
	}
}

// writeSSE emits one server-sent event. A marshal failure is skipped rather
// than killing the stream: one unrenderable event must not end the run's
// live view.
func writeSSE(response *echo.Response, name string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	_, _ = fmt.Fprintf(response, "event: %s\ndata: %s\n\n", name, data)
	response.Flush()
}
