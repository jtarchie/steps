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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/internal/actor"
	"github.com/jtarchie/steps/internal/runview"
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

// liveIdleTimeout closes a stream that has gone quiet, so a peer that went
// away without a FIN — a closed lid — does not hold a poller on the store
// forever. It can only fire on a run still running (a finished one is sent
// `done` the tick it is seen), so the close is SILENT: no `done`, and hx-sse
// reconnects with its Last-Event-ID and resumes exactly where it was. It used
// to send `done` with "idle", which the page could only answer with a reload
// — every five quiet minutes a reader lost the rows they had folded and the
// row they were on, on a job that had done nothing wrong.
//
// A variable so a test can shrink it; see liveBatch.
//
//nolint:gochecknoglobals // a test seam; see liveBatch
var liveIdleTimeout = 5 * time.Minute

// liveRecheckInterval is how often a stream asks again whether its reader may still watch: a stream is authorized once at the door, and a sign-out, a removed account or a narrowed access: rule would otherwise keep receiving a live run until it ends.
//
// A variable so a test can shrink it; see liveBatch.
//
//nolint:gochecknoglobals // a test seam; see liveBatch
var liveRecheckInterval = 30 * time.Second

// liveBatch is how many events one read of the stream takes. The stream pages
// rather than reading a run whole, which is what keeps it working past
// runEventLimit — the page's own bound, and one a delta has no reason to
// inherit.
//
// A variable rather than a constant so a test can shrink it alongside
// runEventLimit. Both have to move together: the test for what happens past
// the page's bound is only ALSO a test of paging while the run outruns one
// batch, so shrinking the bound alone would quietly retire the paging
// coverage while the test kept passing.
//
//nolint:gochecknoglobals // a test seam; see runEventLimit
var liveBatch = 500

// handleRunEvents streams a run's events as server-sent events.
func (s *Server) handleRunEvents(c *echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()
	runID := c.Param("run")

	run, err := requireRun(c, runID)
	if err != nil {
		return err
	}

	after := resumeFrom(c)
	// What the reader is looking at: everything at or before the sequence
	// they resumed from, plus every row sent since on THIS connection. It is
	// what decides whether a fragment is morphed onto a row or appended as a
	// new one, and getting it wrong is silent either way.
	drawn := newSentRows(after)
	// The fold stays open for the life of the connection. Re-reading the run
	// on every tick was the obvious way to render a delta and the wrong one
	// twice over: it re-folded thousands of events 2.5 times a second per
	// watcher, and it stopped at runEventLimit — past that the flush had no
	// touched step in its truncated view, so it wrote nothing at all while
	// holding the socket open and re-arming the idle deadline. A reader
	// watched a frozen transcript on a live connection, with nothing logged.
	folder := runview.NewFolder()

	// Catch the fold up to what the reader is already looking at, without
	// sending any of it: their page was rendered from exactly these events,
	// and the fold has to hold them or a row they can see is missing from
	// every fragment that follows — a container whose children arrived before
	// they connected would re-render with none of them.
	err = s.seedFold(c, runID, after, folder)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	drawn.seed(folder.Steps())

	response := openStream(c)

	ticker := time.NewTicker(livePollInterval)
	defer ticker.Stop()

	deadline := time.NewTimer(liveIdleTimeout)
	defer deadline.Stop()

	lastChecked := time.Now()

	for {
		before := after

		after, err = s.flushEvents(c, run, after, drawn, folder)
		if err != nil {
			return endStream(c, runID, err)
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
		if run.Status != store.RunRunning {
			// The tab's finished mark rides along, rendered here, so a
			// backgrounded tab whose reload is deferred still reports the
			// outcome without the page owning a second copy of the rule.
			finished := runMark(run)
			writeSSE(response, "done", map[string]any{
				"status": run.Status,
				"icon":   string(finished.Favicon()),
				"title":  finished.TitlePrefix() + runTitle(run) + " — steps",
			})

			return nil
		}

		if !s.waitForMore(c, ticker.C, deadline.C, &lastChecked) {
			return nil
		}

		// Read the run BEFORE the next flush, not after it: a run that
		// finishes mid-flush is still seen as running here, so the loop comes
		// round once more and delivers its last events before saying done.
		var ok bool

		run, ok, err = pipeline.Store.FindRunRow(ctx, runID)
		if err != nil {
			return endStream(c, runID, err)
		}

		if !ok {
			// Reaped out from under the reader (retention runs at the end of
			// every build). "gone" rather than the zero row's empty status,
			// which the page read as no outcome at all and skipped the tab
			// mark for — on the one case where the mark is all a backgrounded
			// tab gets.
			writeSSE(response, "done", map[string]any{"status": "gone"})

			return nil
		}
	}
}

// mayStillWatch is authenticate and authorize asked again of a stream already open: the same session, account and role-narrowing questions, with the reader's own cookie.
func (s *Server) mayStillWatch(c *echo.Context) bool {
	who, found := s.sessionActor(c)
	if !found {
		username, password, sent := c.Request().BasicAuth()
		if !sent || !s.auth.matches(username, password) {
			return false
		}

		who = actor.Local(username)
	}

	ctx := c.Request().Context()
	id := s.identityOf(ctx, who)
	role := s.roles.highest(id)

	if target := pipelineOf(c); target != nil {
		role = target.narrow(role, id)
	}

	return role >= RoleViewer
}

// requireRun refuses a run this pipeline does not have, before anything has
// committed the response, and hands back the row the first flush reads.
//
// It has to come first, because everything after it commits: once openStream
// has written the 200 and the event-stream header, echo's error handler
// returns on Committed without so much as a log line, so an unknown run id
// answered 200 and a `done` frame carrying the zero row's empty status — which
// the page reloads on, into a 404 it cannot explain.
func requireRun(c *echo.Context, runID string) (store.RunRow, error) {
	run, ok, err := pipelineOf(c).Store.FindRunRow(c.Request().Context(), runID)
	if err != nil {
		return store.RunRow{}, fmt.Errorf("web: %w", err)
	}

	if !ok {
		return store.RunRow{}, echo.NewHTTPError(http.StatusNotFound, "no such run")
	}

	return run, nil
}

// endStream reports a failure that happened after the response was committed.
//
// Returning the error alone is not enough: openStream has already written the
// 200, so echo's handler sees Committed and returns without rendering OR
// logging. The reader's stream just closes and hx-sse reconnects — and keeps
// reconnecting, because its attempt cap counts only failures to CONNECT, and
// a stream that dies after its 200 resets the count — so a persistent
// post-commit failure is a reconnect every half second per watcher, with the
// operator seeing nothing at all. That is the "green having recorded nothing"
// shape this repo has been bitten by before, and this line is the whole of
// what they get.
//
// Not for a reader who left: the request context is cancelled the moment the
// tab closes, and a store call caught mid-tick reports that as its error.
// Nothing failed, so nothing is logged.
func endStream(c *echo.Context, runID string, err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}

	slog.Warn("web.live.stream_failed", "run", runID, "path", c.Request().URL.Path, "error", err)

	return fmt.Errorf("web: %w", err)
}

// waitForMore holds until the next poll, reporting whether the stream should
// keep going. Silence long enough to trip the deadline ends it without a
// word — see liveIdleTimeout for why a live reader is better off reconnecting
// than being told anything.
//
// A reader who may no longer watch is also ended without a word, like the idle close: their reconnect meets the door and is told why.
func (s *Server) waitForMore(c *echo.Context, tick, deadline <-chan time.Time, lastChecked *time.Time) bool {
	select {
	case <-c.Request().Context().Done():
		return false
	case <-deadline:
		return false
	case <-tick:
	}

	if s.auth == nil || time.Since(*lastChecked) < liveRecheckInterval {
		return true
	}

	*lastChecked = time.Now()

	return s.mayStillWatch(c)
}

// openStream puts the response into server-sent-event mode.
func openStream(c *echo.Context) http.ResponseWriter {
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
func (s *Server) seedFold(c *echo.Context, runID string, seq int64, folder *runFolder) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	for at := int64(0); at < seq; {
		rows, nodes, err := readBatch(ctx, pipeline.Store, runID, at, liveBatch)
		if err != nil {
			return err
		}

		// Anything past the reader's own sequence belongs to the first flush,
		// which has to SEND it.
		for len(rows) > 0 && rows[len(rows)-1].Seq > seq {
			rows = rows[:len(rows)-1]
		}

		if len(rows) == 0 {
			return nil
		}

		folder.Add(rows, nodes)

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
func resumeFrom(c *echo.Context) int64 {
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
// What a flush sends is the smallest unit that leaves the page right, per
// row: a whole row only when the reader lacks it or it closed, a turn's own
// markup when an agent spoke, and a container's attributes and head — its
// shell — when a step under it came or went, because that changes its rollup
// and nothing publishes an event on the container. Sending the root's whole
// subtree instead was measured quadratic: flush k of an agent step re-shipped
// k turns, and a container re-shipped every child on each event under it.
// See framer.
func (s *Server) flushEvents(
	c *echo.Context, run store.RunRow, after int64, drawn *sentRows, folder *runFolder,
) (int64, error) {
	// Drained, not read once: a tick reads a bounded batch, and a run that
	// recorded more than one batch between ticks — or that had already
	// finished when the reader connected — would otherwise deliver a batch
	// and close, leaving a transcript that ends mid-run with no sign it was
	// cut. Each batch is its own message, so what a reconnect resumes from
	// stays exact. A short page is the end: the store had nothing more when
	// it answered, so asking again would only buy an empty read.
	for {
		next, more, err := s.flushBatch(c, run, after, drawn, folder)
		if err != nil {
			return next, err
		}

		after = next

		if !more {
			return after, nil
		}
	}
}

// flushBatch folds and sends one page of events, reporting the new high-water
// mark and whether the page was full — in which case another may follow.
func (s *Server) flushBatch(
	c *echo.Context, run store.RunRow, after int64, drawn *sentRows, folder *runFolder,
) (int64, bool, error) {
	// Only the nodes THIS batch names: a finished agent step's answer and
	// trajectory live in its node, and the rest of the run's nodes are
	// already folded in.
	rows, nodes, err := readBatch(c.Request().Context(), pipelineOf(c).Store, run.ID, after, liveBatch)
	if err != nil {
		return after, false, err
	}

	if len(rows) == 0 {
		return after, false, nil
	}

	more := len(rows) == liveBatch

	// The fold says what it touched, rather than this reading it off the rows:
	// the two disagree for a sub-agent's turns, which name a step no row ever
	// opened. See runview.Folder.Add.
	changes := folder.Add(rows, nodes)
	after = rows[len(rows)-1].Seq

	// Drawn as in flight even once the row has ended: a stream catching up
	// after the run finished would otherwise draw every row still waiting
	// for its close as unreported, then flip it back when the close arrives
	// later in the same drain. The done that ends the stream reloads the
	// page, and the reload is what settles a row nothing closed.
	inFlight := run
	inFlight.Status = store.RunRunning

	view := runView{Transcript: folder.View(inFlight)}

	// The same decoration assembleRun gives a full-page render (run.go)
	// — a running agent step's countdown is drawn inside the per-step row,
	// which this stream sends, so without this a step that starts (or is
	// re-sent for any other reason) while a connection is already open never
	// shows one. See attachStepDeadlines' own doc comment.
	attachStepDeadlines(&view, pipelineOf(c).Config(), run.JobName, run.ConfigSHA)

	// ONE message per flush, however many rows changed. The id a browser
	// resends on a reconnect names the last message it applied, so a flush
	// split across several messages could be resumed from the middle of
	// itself: the rows in the frames that never arrived would be skipped, and
	// an appended row skipped that way is gone for the rest of the run —
	// every later flush renders it as a swap onto an id the page never drew.
	// Whole flush or nothing.
	frame := framer{
		server: s,
		// The `step` template reads one thing off the nav — the slug its
		// node links are scoped by — and s.nav() would buy that with the two
		// pending counts nothing in a row displays, 2.5 times a second per
		// watcher.
		page:    map[string]any{"Nav": navData{Current: pipelineOf(c).Slug}, "Run": view},
		drawn:   drawn,
		changes: changes,
	}

	for _, root := range view.Roots {
		err := frame.visit(root, nil)
		if err != nil {
			return after, false, err
		}
	}

	if frame.out.Len() == 0 {
		return after, more, nil
	}

	writeFrame(c.Response(), after, frame.out.String())

	return after, more, nil
}

// eventReader is what one page of a run costs: the events, and the cache the
// hashes in them name.
type eventReader interface {
	store.Events
	store.Cache
}

// readBatch is one page of a run's events with the nodes they name — the
// unit every fold is fed, whether it is the page's one read, the stream's
// seed or its delta.
func readBatch(
	ctx context.Context, st eventReader, runID string, after int64, limit int,
) ([]store.RunEventRow, map[string]store.NodeRow, error) {
	rows, err := st.RunEvents(ctx, runID, after, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("web: %w", err)
	}

	nodes, err := st.NodesByHash(ctx, hashesOf(rows))
	if err != nil {
		return nil, nil, fmt.Errorf("web: %w", err)
	}

	return rows, nodes, nil
}

// hashesOf is the node hashes a batch of events names, deduped: a hash
// repeats across a run's events (and across the steps a chain-skip
// swallowed), and each repeat would add a redundant bind parameter to the IN
// clause it feeds.
func hashesOf(rows []store.RunEventRow) []string {
	seen := map[string]bool{}
	hashes := make([]string, 0, len(rows))

	for _, row := range rows {
		if row.Hash == "" || seen[row.Hash] {
			continue
		}

		seen[row.Hash] = true

		hashes = append(hashes, row.Hash)
	}

	return hashes
}
