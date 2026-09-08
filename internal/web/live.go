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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
func (s *Server) handleRunEvents(c echo.Context) error {
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
	folder := newRunFolder()

	// Catch the fold up to what the reader is already looking at, without
	// sending any of it: their page was rendered from exactly these events,
	// and the fold has to hold them or a row they can see is missing from
	// every fragment that follows — a container whose children arrived before
	// they connected would re-render with none of them.
	err = s.seedFold(c, runID, after, folder)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	drawn.seed(folder.run.Steps)

	response := openStream(c)

	ticker := time.NewTicker(livePollInterval)
	defer ticker.Stop()

	deadline := time.NewTimer(liveIdleTimeout)
	defer deadline.Stop()

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
		if run.Status != "running" {
			writeSSE(response, "done", map[string]any{"status": run.Status})

			return nil
		}

		if !waitForMore(ctx, ticker.C, deadline.C) {
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

// requireRun refuses a run this pipeline does not have, before anything has
// committed the response, and hands back the row the first flush reads.
//
// It has to come first, because everything after it commits: once openStream
// has written the 200 and the event-stream header, echo's error handler
// returns on Committed without so much as a log line, so an unknown run id
// answered 200 and a `done` frame carrying the zero row's empty status — which
// the page reloads on, into a 404 it cannot explain. It also keeps seedFold
// from paging a run this pipeline does not own: RunEvents filters on run_id
// alone, and FindRunRow is the pipeline-scoped question.
func requireRun(c echo.Context, runID string) (store.RunRow, error) {
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
func endStream(c echo.Context, runID string, err error) error {
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
func waitForMore(ctx context.Context, tick, deadline <-chan time.Time) bool {
	select {
	case <-ctx.Done():
		return false
	case <-deadline:
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
// What a flush sends is the smallest unit that leaves the page right, per
// row: a whole row only when the reader lacks it or it closed, a turn's own
// markup when an agent spoke, and a container's attributes and head — its
// shell — when a step under it came or went, because that changes its rollup
// and nothing publishes an event on the container. Sending the root's whole
// subtree instead was measured quadratic: flush k of an agent step re-shipped
// k turns, and a container re-shipped every child on each event under it.
// See framer.
func (s *Server) flushEvents(
	c echo.Context, run store.RunRow, after int64, drawn *sentRows, folder *runFolder,
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
	c echo.Context, run store.RunRow, after int64, drawn *sentRows, folder *runFolder,
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
	// opened. See runFolder.add.
	changes := folder.add(rows, nodes)
	after = rows[len(rows)-1].Seq

	view := folder.view(run)

	// The same decoration assembleRun gives a full-page render (handlers.go)
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

// readBatch is one page of a run's events with the nodes they name — the
// unit every fold is fed, whether it is the page's one read, the stream's
// seed or its delta.
func readBatch(
	ctx context.Context, st store.Store, runID string, after int64, limit int,
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

// framer builds one flush's frame by walking the tree and choosing, per row,
// the smallest fragment that leaves the reader's page identical to a reload.
//
// Four fragments exist, and the choice is made top-down so that a row sent
// whole covers everything under it:
//
//   - a row the reader lacks is sent whole and APPENDED — into its parent's
//     substeps, or the transcript for a root;
//   - a row that opened, closed or printed is sent whole and MORPHED over
//     the copy the reader has, because those change its body in ways an
//     append cannot express (the answer an agent ends on is the turn the
//     page then drops);
//   - a running agent's new turns are APPENDED to its body, each one drawn
//     by the same `turn` template the page uses, and the row is not re-sent;
//   - a container whose descendants came or went gets its SHELL — the
//     attributes and head, where the rollup and the active rail live — and
//     nothing it holds, because the children that did not move are already
//     right and the ones that did are sent on their own.
//
// The shell is the fragment that depends on htmx: an outerMorph carrying
// hx-morph-skip-children syncs the element's attributes and leaves its
// children alone. See the template.
type framer struct {
	server  *Server
	page    map[string]any
	drawn   *sentRows
	changes map[string]stepChange
	out     strings.Builder
}

func (f *framer) visit(step *stepView, parent *stepView) error {
	switch {
	case !f.drawn.has(step):
		f.retract(step)

		return f.appendRow(step, parent)
	case f.wantsWhole(step):
		f.retract(step)

		return f.morphRow(step)
	}

	if f.changes[step.Key()].Turns > 0 {
		err := f.appendTurns(step)
		if err != nil {
			return err
		}
	}

	// Decided before the children are visited: a visit marks what it sends
	// as drawn, and this asks what the reader was missing.
	if descendantsMoved(step, f.drawn, f.changes) {
		err := f.render("stepshell", stepCtx{Page: f.page, Step: step, OOB: true})
		if err != nil {
			return err
		}
	}

	for _, child := range step.Children {
		err := f.visit(child, step)
		if err != nil {
			return err
		}
	}

	return nil
}

// wantsWhole reports a row the reader has that only a whole re-send leaves
// right: it opened, closed or printed; every child under it is new, so their
// copy holds no substeps element to append into; or it gained turns that
// cannot be appended — their copy has no body yet (the row was drawn before
// its first turn), or the step has finished, and the page then drops the
// turn that repeats the answer, which is a change to what is already drawn.
func (f *framer) wantsWhole(step *stepView) bool {
	change := f.changes[step.Key()]
	if change.Opened || change.Closed || change.Other {
		return true
	}

	if change.Turns > 0 && (f.drawn.turns[step.Key()] == 0 || !step.Running()) {
		return true
	}

	return f.firstChildren(step)
}

// retract deletes, ahead of a whole send of step, every row the reader holds
// loose that the send is about to draw again inside it — in the same frame
// and BEFORE it, because htmx applies out-of-band swaps in the order they
// arrive. See adopted(). Both whole senders need it: a container the reader
// lacks is appended, and one they have is morphed, and either re-draws its
// subtree; only the append used to retract, so a container morphed whole
// over a loose grandchild left two rows with one id on the page.
func (f *framer) retract(step *stepView) {
	for _, orphan := range adopted(step, f.drawn.has) {
		fmt.Fprintf(&f.out, `<div id="%s" hx-swap-oob="delete"></div>`, orphan.Anchor())
	}
}

// appendRow sends a row the reader does not have, whole, to be appended
// under its parent — or to the transcript when it has none. htmx drops an
// out-of-band swap at a missing id without a word, so a missing row is never
// morphed.
func (f *framer) appendRow(step *stepView, parent *stepView) error {
	target := "#transcript"
	if parent != nil {
		target = "#" + parent.Anchor() + "_substeps"
	}

	// beforeend strips this wrapper and appends what is inside it, which is
	// how a row that does not exist yet gets onto the page at all.
	fmt.Fprintf(&f.out, `<div hx-swap-oob="beforeend:%s">`, target)

	err := f.render("step", stepCtx{Page: f.page, Step: step})
	if err != nil {
		return err
	}

	f.out.WriteString(`</div>`)
	f.drawn.drew(step)

	return nil
}

// morphRow sends a row the reader has, whole, to be morphed over their copy.
func (f *framer) morphRow(step *stepView) error {
	err := f.render("step", stepCtx{Page: f.page, Step: step, OOB: true})
	if err != nil {
		return err
	}

	f.drawn.drew(step)

	return nil
}

// appendTurns sends the turns the reader has not seen, appended to the body
// of a running agent's row. wantsWhole has already ruled out the rows this
// cannot be done to.
func (f *framer) appendTurns(step *stepView) error {
	shown := f.drawn.turns[step.Key()]

	fmt.Fprintf(&f.out, `<div hx-swap-oob="beforeend:#%s_body">`, step.Anchor())

	for _, turn := range step.Turns[shown:] {
		err := f.render("turn", turn)
		if err != nil {
			return err
		}
	}

	f.out.WriteString(`</div>`)
	f.drawn.turns[step.Key()] = len(step.Turns)

	return nil
}

// firstChildren reports a step whose children are all new to the reader:
// their copy of the row was drawn with none, so it carries no substeps
// element for an append to land in.
func (f *framer) firstChildren(step *stepView) bool {
	if len(step.Children) == 0 {
		return false
	}

	for _, child := range step.Children {
		if f.drawn.has(child) {
			return false
		}
	}

	return true
}

func (f *framer) render(name string, data any) error {
	tmpl, ok := f.server.renderer.pages["run"]
	if !ok {
		return fmt.Errorf("web: no run template to render %q with", name)
	}

	err := tmpl.ExecuteTemplate(&f.out, name, data)
	if err != nil {
		return fmt.Errorf("web: could not render %s: %w", name, err)
	}

	return nil
}

// descendantsMoved reports whether a step under this one came or went this
// flush — the changes a container's rollup and active rail show, and the
// only ones that reach a row from below. A turn or an output under it does
// not.
func descendantsMoved(step *stepView, drawn *sentRows, changes map[string]stepChange) bool {
	for _, child := range step.Children {
		change := changes[child.Key()]
		if !drawn.has(child) || change.Opened || change.Closed {
			return true
		}

		if descendantsMoved(child, drawn, changes) {
			return true
		}
	}

	return false
}

// sentRows answers, for one connection, what the reader's page already holds:
// which rows, and how many turns each shows.
//
// A row is theirs if the event that put it on the page came at or before the
// sequence they resumed from — the page they are looking at was rendered from
// exactly those events — or if this connection has since sent it. Asking
// instead whether a step.started arrived in the current flush was almost the
// same question and wrong for the case that has no start at all: a step
// swallowed by a chain skip is opened by its step.skipped, so its row was
// swapped over an id nothing had drawn and vanished.
//
// Asking and recording are separate calls rather than one predicate that
// marks what it is asked about: adopted() below asks about rows it is not
// sending, and marking those would turn their own later append into a swap at
// an id the page never drew — which htmx drops in silence.
type sentRows struct {
	origin int64
	sent   map[string]bool
	// turns is how many of a row's turns the reader's copy shows, which is
	// where the next append starts. Seeded from the fold for the rows the
	// page drew, and set by every whole-row send after that.
	turns map[string]int
}

func newSentRows(origin int64) *sentRows {
	return &sentRows{origin: origin, sent: map[string]bool{}, turns: map[string]int{}}
}

// has reports whether the reader's page already carries this row.
func (s *sentRows) has(step *stepView) bool {
	return step.FirstSeq <= s.origin || s.sent[step.Key()]
}

// drew records a row this connection has sent whole — and with it everything
// under it, because a row goes over with its subtree and a child drawn that
// way must not be appended a second time on its own.
func (s *sentRows) drew(step *stepView) {
	s.sent[step.Key()] = true
	s.turns[step.Key()] = len(step.Turns)

	for _, child := range step.Children {
		s.drew(child)
	}
}

// seed records what the page drew for each row the reader already has, so
// the first append to any of them starts after the turns they can see.
func (s *sentRows) seed(steps []*stepView) {
	for _, step := range steps {
		s.turns[step.Key()] = len(step.Turns)
	}
}

// adopted names the rows the reader already has LOOSE that a whole send of
// this step is about to draw again inside it.
//
// A step is hung under its container only once the container has a row of its
// own, and the two events do not arrive in that order: a container swallowed
// by a chain skip is opened by its own step.skipped, which the store records
// after the steps that ran inside it (the same ordering that makes
// run_events.parent_step_id a deliberate non-foreign-key). Until then the
// child is a root and the reader carries it at the transcript's top level, so
// drawing the container would put a SECOND element with that id on the page
// — and every later out-of-band swap resolves to the first match, leaving the
// orphan updating and the real row frozen.
//
// A row is loose exactly when the reader has it but not its parent: a drawn
// parent's drawn child is INSIDE it — the page hung it there, or an earlier
// send carried or adopted it — and must not be retracted, or the reader
// loses the fold state on a row that never moved. So a drawn child of a drawn
// step is searched, not named; a drawn child of a step the reader lacks is
// the orphan, and whatever is under it came with it.
func adopted(step *stepView, has func(*stepView) bool) []*stepView {
	var found []*stepView

	for _, child := range step.Children {
		if has(child) && !has(step) {
			found = append(found, child)

			continue
		}

		found = append(found, adopted(child, has)...)
	}

	return found
}

// frameLines cuts a fragment into the data lines one SSE message carries.
//
// html/template escapes the five HTML metacharacters and NOT the carriage
// return, but SSE ends a line on CRLF, CR *or* LF — so a lone CR in a step's
// output (every progress bar writes them) ends the data line early and the
// markup after it is read as SSE fields. `\r\revent: done\rdata: 0\r\r` on a
// task's stdout is a forged run-completion frame: it closes the reader's
// stream and paints the tab red on a job that is still running.
//
// Every line ending becomes a data line, and a lone CR is a line ending too —
// not because SSE says so but because HTML does: the parser turns every CR
// in a page's bytes into LF before it tokenizes, so on reload a progress
// bar's overwrites are three lines. Sent as the entity instead, the CR
// survived into the live DOM as U+000D, which CSS draws as a space, and the
// reader watched one run-on line become three at the closing reload.
func frameLines(html string) []string {
	normalized := strings.ReplaceAll(html, "\r\n", "\n")

	return strings.Split(strings.ReplaceAll(normalized, "\r", "\n"), "\n")
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
