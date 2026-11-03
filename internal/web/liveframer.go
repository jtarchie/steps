package web

import (
	"fmt"
	"strings"
)

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

	err := f.appendNew(step)
	if err != nil {
		return err
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

	// The same for chunks: their copy has no live block until the first one is drawn with the row.
	if change.Streamed > 0 && (f.drawn.streamed[step.Key()] == 0 || !step.Running()) {
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
		err := f.render("turn", newTurnCtx(f.page, step, turn))
		if err != nil {
			return err
		}
	}

	f.out.WriteString(`</div>`)
	f.drawn.turns[step.Key()] = len(step.Turns)

	return nil
}

// appendNew sends what a row the reader already has can take by append alone: an agent's new turns, a task's new chunks.
func (f *framer) appendNew(step *stepView) error {
	if f.changes[step.Key()].Turns > 0 {
		err := f.appendTurns(step)
		if err != nil {
			return err
		}
	}

	if f.changes[step.Key()].Streamed > 0 {
		return f.appendStreamed(step)
	}

	return nil
}

// appendStreamed sends the output chunks the reader has not seen, appended to the live block of a running step's row — the row itself would carry every chunk so far, and re-sending it per chunk is quadratic in the output.
func (f *framer) appendStreamed(step *stepView) error {
	shown := f.drawn.streamed[step.Key()]

	fmt.Fprintf(&f.out, `<div hx-swap-oob="beforeend:#%s_live">`, step.Anchor())

	for _, chunk := range step.Streaming[shown:] {
		err := f.render("chunk", chunk)
		if err != nil {
			return err
		}
	}

	f.out.WriteString(`</div>`)
	f.drawn.streamed[step.Key()] = len(step.Streaming)

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
	// streamed is turns' twin for output chunks.
	streamed map[string]int
}

func newSentRows(origin int64) *sentRows {
	return &sentRows{origin: origin, sent: map[string]bool{}, turns: map[string]int{}, streamed: map[string]int{}}
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
	s.streamed[step.Key()] = len(step.Streaming)

	for _, child := range step.Children {
		s.drew(child)
	}
}

// seed records what the page drew for each row the reader already has, so
// the first append to any of them starts after the turns they can see.
func (s *sentRows) seed(steps []*stepView) {
	for _, step := range steps {
		s.turns[step.Key()] = len(step.Turns)
		s.streamed[step.Key()] = len(step.Streaming)
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
