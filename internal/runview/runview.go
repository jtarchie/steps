// Package runview folds a run's recorded events into the step tree a reader
// sees. It is the one fold both front ends draw from — the browser transcript
// (internal/web) and the terminal — so the two cannot disagree about what a
// run looked like.
//
// A run's events arrive as a flat ordered log (because that is what actually
// happened, including concurrent steps interleaving), and a reader needs them
// as steps with their traffic underneath. Everything here does that reshaping
// and nothing else — no queries, no rendering — so the shapes are testable
// without a server or a terminal.
package runview

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// Transcript is the part of a run the event log alone determines.
type Transcript struct {
	Run store.RunRow
	// Steps is every step of the run in start order, flat. The page renders
	// Roots instead; this is what the run-level questions (what changed, what
	// was cached) are still asked of, because they are about the run and not
	// about its shape.
	Steps []*Step
	// Roots are the steps at the top of the plan, each holding its subtree.
	Roots    []*Step
	JobError string
	// Notes are the run's notes that belong to no step it recorded: said at the job level, or by a step whose start never reached the log.
	Notes []Note
	// LastSeq is the highest event sequence this view already renders, and it
	// is what the live stream must resume AFTER.
	//
	// Without it the stream opened at ?after=0 and replayed events the page
	// had already drawn, appending a second copy of every turn and every line
	// of output a run had produced before the tab was opened — visible on
	// exactly the page a person opens while a run is in flight.
	LastSeq int64
}

// Running reports a run still in flight, which is what decides whether the page opens a live event stream.
func (r Transcript) Running() bool { return r.Run.Status == "running" }

// HasSkipped reports whether any step replayed from cache. The page explains
// folding only when there is something folded — an explanation of a mechanism
// the reader cannot see on the page is noise.
func (r Transcript) HasSkipped() bool {
	for _, step := range r.Steps {
		if step.Skipped() {
			return true
		}
	}

	return false
}

// Step is one step of a run, with whatever the step produced beneath it —
// including, for a block step, the steps that ran inside it.
type Step struct {
	// ID and ParentID are the run's display tree (see events.Event). Zero on
	// a run recorded before the tree existed, which folds back to the flat
	// list this page used to be.
	ID       int64
	ParentID int64
	// Children are the steps that ran inside this one, in start order.
	Children []*Step
	// FirstSeq is the sequence of the event that PUT this step on the page,
	// which is what the live stream asks when deciding whether the reader
	// already has the row. Not always a step.started: a step swallowed by a
	// chain skip is opened by its step.skipped, and asking about starts alone
	// meant such a row was swapped over an id that was never drawn — an
	// out-of-band swap htmx drops in silence.
	FirstSeq int64
	Index    int
	Name     string
	Kind     string
	Status   string
	Hash     string
	Reason   string // why it was skipped, when it was
	Error    string
	// Worker is where the step's commands ran, "tag (address)", empty for
	// this machine. Drawn because a red step on a fleet otherwise names no
	// machine — see events.Event.Worker.
	Worker   string
	Duration time.Duration
	Started  time.Time
	// Deadline is when a running agent step's resolved timeout: expires —
	// Started plus the ceiling. Never set by the fold: the caller holding the configuration fills it in (web's attachStepDeadlines).
	// Zero (HasDeadline false) for a finished or non-agent step, an
	// unlimited timeout, or a run whose configuration no longer matches
	// what is loaded — the same "unknowable, not uncapped" reasoning
	// web's runView.Ceilings documents.
	Deadline time.Time
	// Turns is agent conversation traffic that arrived while this step was
	// the one running. Empty for every other kind.
	Turns []Turn
	// Result is the node's recorded result, decoded — the verdict, the
	// response, the trajectory. Nil when the step recorded none.
	Result map[string]any
	// Outputs is what the step printed, one entry per output event.
	//
	// A slice rather than a string because a step with attempts: publishes one
	// output per attempt, and the live stream appends a block for each. Held as
	// a single overwritten string, the page a reader watched three attempts on
	// would silently drop two of them at the closing reload.
	Outputs []string
	// Notes is what the step's machinery said about it, in order.
	Notes []Note
}

// Note is one TypeStepNote, as a reader sees it.
type Note struct {
	Level string
	Text  string
	At    time.Time
}

// Warn reports a note worth a reader's attention, not only their record.
func (n Note) Warn() bool { return n.Level == events.NoteWarn }

// Running reports a step that started and has not reported an end.
func (s Step) Running() bool { return s.Status == "" || s.Status == "running" }

// Elapsed is how long a running step has been running, for the row's own
// clock. The page's timer script keeps it counting; this is what it reads
// before the first tick, and what a reader with scripting off sees.
func (s Step) Elapsed() time.Duration {
	if s.Started.IsZero() {
		return 0
	}

	return time.Since(s.Started)
}

// HasDeadline reports whether Deadline could be resolved for this step.
func (s Step) HasDeadline() bool { return !s.Deadline.IsZero() }

// Remaining is how long until Deadline, for the row's own clock — the
// countdown counterpart to Elapsed, floored at 0 rather than going negative
// for the rare page load that lands after the deadline technically passed
// but before the step's own end event has arrived.
func (s Step) Remaining() time.Duration {
	if s.Deadline.IsZero() {
		return 0
	}

	if remaining := time.Until(s.Deadline); remaining > 0 {
		return remaining
	}

	return 0
}

// Skipped reports a step that did not execute.
func (s Step) Skipped() bool { return s.Status == "skipped" }

// Failed reports a step that ended badly, by any classification.
func (s Step) Failed() bool {
	return s.Status == "failed" || s.Status == "errored" || s.Status == "aborted"
}

// Container reports a step that ran other steps inside it.
func (s Step) Container() bool { return len(s.Children) > 0 }

// Active reports a step still running, or holding something that is.
//
// It is what lights the rail down the branch the work is actually on, so a
// reader who has folded half the page still knows where to look. Recursive
// rather than a flag set at fold time, because a container's own status stays
// running until every child has finished — the two answers agree, and this
// one needs no second pass to maintain.
func (s Step) Active() bool {
	if !s.Running() {
		return false
	}

	if !s.Container() {
		return true
	}

	for _, child := range s.Children {
		if child.Active() {
			return true
		}
	}

	// A container whose children have all finished while it has not is
	// between its last child and its own finish event. Nothing is running
	// inside it, so nothing about it should read as running.
	return false
}

// Tally counts how a container's subtree came out, for the row itself. A
// folded block still has to answer "where does this stand", and the rows that
// would otherwise answer are folded away with it.
type Tally struct {
	Cells   int
	Passed  int
	Failed  int
	Running int
	Skipped int
}

// Empty reports a Tally with nothing to say, which is not rendered.
//
// A container holding ONE step says nothing a reader cannot read off that
// step's own row — a try: wrapping a task would otherwise carry a permanent
// "1 step · 1 passed" that is pure furniture. Unless something inside went
// wrong: a failure has to survive the fold, however few steps it took.
func (r Tally) Empty() bool { return r.Cells == 0 || (r.Cells == 1 && r.Failed == 0) }

// Rollup summarises the step's DIRECT children — the unit a reader counts.
// A matrix reports its cells, not the tasks and agents inside them, which is
// the number the pipeline itself printed when it fanned out.
func (s Step) Rollup() Tally {
	var out Tally

	for _, child := range s.Children {
		out.Cells++

		switch {
		case child.Skipped():
			out.Skipped++
		case child.Failed():
			out.Failed++
		case child.Running():
			out.Running++
		default:
			out.Passed++
		}
	}

	return out
}

// HasBody reports whether the step produced anything to show under its own
// row — as distinct from the steps that ran INSIDE it, which are a subtree.
//
// The two are separate because the body is rendered at all only when there is
// one: an empty .stepbody still carries its padding, which on a container
// opened a visible gap between the block and the first step inside it.
func (s Step) HasBody(jobError string) bool {
	return len(s.Turns) > 0 ||
		len(s.Trajectory()) > 0 ||
		len(s.Outputs) > 0 ||
		len(s.Notes) > 0 ||
		s.DistinctError(jobError) != "" ||
		s.Response() != "" ||
		s.Note() != "" ||
		s.Reason != ""
}

// HasDetail reports whether the step has anything to show when expanded.
// A step with no body must not be foldable: an expandable row that opens onto
// nothing reads as a broken page, and a chevron that promises detail there
// isn't is worse than no chevron.
func (s Step) HasDetail(jobError string) bool {
	return s.Container() || s.HasBody(jobError)
}

// DistinctError is the step's error, or "" when it is the same text the run
// already leads with. A failing step's error is usually what the job error IS
// (the plan wraps it and returns it), and printing one long message twice on
// the page a reader reaches while triaging is exactly where noise costs most.
func (s Step) DistinctError(jobError string) string {
	if s.Error == "" || strings.Contains(jobError, s.Error) {
		return ""
	}

	return s.Error
}

// Anchor is the step's own id in the page, and the target of the # link
// beside its name.
//
// Built on the step occurrence rather than on (index, name), because those
// two are shared: a try: and the step inside it produced the SAME anchor, so
// the page carried duplicate ids and a link pasted at someone opened the
// wrapper instead of the step they meant. The slug stays for a human reading
// the URL; the id is what makes it point at one row.
func (s Step) Anchor() string {
	if s.ID == 0 {
		return fmt.Sprintf("step-%d-%s", s.Index, Slug(s.Name))
	}

	return fmt.Sprintf("step-%d-%s", s.ID, Slug(s.Name))
}

// Key identifies this row to the live stream, which must find the row the
// server already drew rather than appending a second one. Mirrors stepKey.
func (s Step) Key() string {
	if s.ID != 0 {
		return "#" + strconv.FormatInt(s.ID, 10)
	}

	return fmt.Sprintf("%d/%s", s.Index, s.Name)
}

// Call is one recorded tool call read back from a node's result.
type Call struct {
	Name     string
	OK       bool
	ArgsJSON string
}

// Trajectory is the tool calls a step's node recorded, for the steps whose
// conversation never reached the event bus.
//
// A CLI-backed agent (source.model: "@claude/sonnet") owns its own tool loop
// in a subprocess, so it publishes no turns — but the calls it made are
// parsed out of the CLI's stream and stored in the node's result either way.
// The page showed the final answer and nothing about how it got there, while
// the record of exactly that sat one field away in a map it had already
// decoded.
//
// Empty when the step DID publish turns: those are the same calls, live and
// in order, and rendering both would show every tool call twice.
func (s Step) Trajectory() []Call {
	if len(s.Turns) > 0 || s.Result == nil {
		return nil
	}

	recorded, _ := s.Result["trajectory"].([]any)

	calls := make([]Call, 0, len(recorded))

	for _, entry := range recorded {
		call, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		name, _ := call["name"].(string)
		if name == "" {
			continue
		}

		// A call whose "ok" is absent reads as having run, matching how the
		// CLI stream records one it never saw a result block for.
		succeeded, present := call["ok"].(bool)

		calls = append(calls, Call{
			Name:     name,
			OK:       succeeded || !present,
			ArgsJSON: encodeArgs(call["args"]),
		})
	}

	return calls
}

// encodeArgs renders a recorded call's arguments back to JSON, so they render
// through the same jsonValue path a live tool call's do.
func encodeArgs(args any) string {
	if args == nil {
		return ""
	}

	encoded, err := json.Marshal(args)
	if err != nil {
		return ""
	}

	return string(encoded)
}

// Verdict pulls the routing verdict out of an agent step's result.
func (s Step) Verdict() string { return s.resultString("verdict") }

// Note pulls the verdict's note.
func (s Step) Note() string { return s.resultString("note") }

// WrappedUp reports a step whose conversation ran out of turns and was asked
// to answer from what it had already gathered.
//
// Worth its own marker for the reason the runner records it at all: the
// answer is degraded, and afterwards it is indistinguishable from a confident
// one. It is the counterpart of Truncated() on the spend panel — that one is
// the model's output being cut off mid-sentence, this one is the step's turn
// budget running out — and reading them together is how an author tells "the
// model had nothing more to say" from "the model was stopped".
func (s Step) WrappedUp() bool {
	if s.Result == nil {
		return false
	}

	wrapped, _ := s.Result["wrapped_up"].(bool)

	return wrapped
}

// Response pulls the agent's final answer.
func (s Step) Response() string { return s.resultString("response") }

func (s Step) resultString(key string) string {
	if s.Result == nil {
		return ""
	}

	value, _ := s.Result[key].(string)

	return value
}

// Conversation is the step's turns with the one redundant turn dropped: the
// model's last text is normally the answer the Response block already shows in
// full, and printing a whole response twice on one page — once mid-transcript,
// once labeled — is noise exactly where a reader is trying to find the answer.
//
// Only that turn, and only when it matches: a model's running commentary
// mid-conversation is not the answer, and a response the model never said in a
// text turn (a wrapped-up conversation, a verdict-only step) still has to
// appear.
func (s Step) Conversation() []Turn {
	response := strings.TrimSpace(s.Response())
	if response == "" || len(s.Turns) == 0 {
		return s.Turns
	}

	// The last text, not the last turn: a model that emits text AND a tool call
	// in one message records the result after the text, so keying on the
	// trailing turn let the answer through twice.
	for i := len(s.Turns) - 1; i >= 0; i-- {
		if s.Turns[i].Type != "agent_text" {
			continue
		}

		if strings.TrimSpace(s.Turns[i].Text) != response {
			return s.Turns
		}

		return append(s.Turns[:i:i], s.Turns[i+1:]...)
	}

	return s.Turns
}

// Turn is one piece of agent conversation traffic.
type Turn struct {
	Type   string
	Text   string
	Name   string
	Detail string
	Depth  int
	At     time.Time
}

// Nested reports a turn belonging to a delegated sub-agent rather than the
// step's own conversation.
func (t Turn) Nested() bool { return t.Depth > 0 }

// IsMessage reports a turn that was SENT to the model — a system prompt or a
// user message — as opposed to one it wrote. The template branches on this
// rather than on a third copy of the type-string list, so run.html and
// node.html can agree on which turns are shown literally and coloured
// versus rendered as prose (see prose.go's file comment).
func (t Turn) IsMessage() bool {
	return t.Type == events.TypeAgentSystem || t.Type == events.TypeAgentUser
}

// IsModelText reports the model's own running commentary or final answer.
func (t Turn) IsModelText() bool { return t.Type == events.TypeAgentText }

// Build folds a run's ordered events into steps.
//
// Steps are keyed by (index, name) rather than index alone: an across: cell
// and its siblings all report their parent's plan index, and collapsing them
// onto it would render a fan-out as one flickering step instead of the
// several concurrent ones it is.
func Build(run store.RunRow, rows []store.RunEventRow, results map[string]store.NodeRow) Transcript {
	folder := NewFolder()
	folder.Add(rows, results)

	return folder.View(run)
}

// Folder is the fold itself, kept open.
//
// The page reads a run once and folds every event in one go; the live stream
// folds each flush into the view it already has, which is the only way it can
// render a delta without re-reading the whole run 2.5 times a second — and
// without stopping dead at runEventLimit, which is what a re-read does to a
// run longer than that. Both go through this type rather than through two
// folds that would have to agree.
type Folder struct {
	run   Transcript
	index map[string]int
}

// NewFolder is an empty fold.
func NewFolder() *Folder {
	return &Folder{index: map[string]int{}}
}

// Change is what one batch of events did to a step's row, in the terms
// the live stream decides by: whether the row itself came or went, how many
// turns were hung under it, and whether anything else about it moved.
//
// Turns are counted rather than flagged because they are the one change the
// stream can send as an APPEND — the turn's own markup, not the row's — and
// only when nothing else about the row moved in the same batch. An agent step
// that spoke three hundred times used to be re-sent whole on each of them.
type Change struct {
	Opened bool
	Closed bool
	Other  bool
	Turns  int
}

// Merge folds a later change on the same row into this one.
func (c Change) Merge(other Change) Change {
	return Change{
		Opened: c.Opened || other.Opened,
		Closed: c.Closed || other.Closed,
		Other:  c.Other || other.Other,
		Turns:  c.Turns + other.Turns,
	}
}

// Any reports whether the change moved anything a reader would see.
func (c Change) Any() bool { return c.Opened || c.Closed || c.Other || c.Turns > 0 }

// Add folds one batch of events in, in order, reporting what changed on
// which steps' rows as a result.
//
// The fold reports it rather than the caller reading it off the events,
// because the two disagree exactly where it matters: a sub-agent's turns
// carry the CHILD's name, and attachTurn hangs them on the plan step still
// running. A caller deriving the set from stepKey(row) alone therefore holds
// a key belonging to no row — and the live stream, which sends only the rows
// its batch named, sent NOTHING for the whole of a sub-agent conversation.
func (f *Folder) Add(rows []store.RunEventRow, results map[string]store.NodeRow) map[string]Change {
	touched := make(map[string]Change, len(rows))

	for _, row := range rows {
		if row.Seq > f.run.LastSeq {
			f.run.LastSeq = row.Seq
		}

		if position, change := f.fold(row, results); change.Any() {
			key := f.run.Steps[position].Key()
			touched[key] = touched[key].Merge(change)
		}

		// The job's error is read by every row, not one: a step's own error
		// line is dropped once the job's error quotes it (DistinctError), and
		// with it, for a step that printed nothing else, the body and the
		// toggle. Those rows are re-drawn, or the reader keeps an error line a
		// reload no longer shows.
		if row.Type == events.TypeJobFinished && row.Text != "" {
			for _, step := range f.run.Steps {
				if step.Error != "" && strings.Contains(row.Text, step.Error) {
					touched[step.Key()] = touched[step.Key()].Merge(Change{Other: true})
				}
			}
		}
	}

	return touched
}

// fold applies one event, reporting the position of the step whose row it
// changed — which is not always the step the event names — and how. See Add.
func (f *Folder) fold(row store.RunEventRow, results map[string]store.NodeRow) (int, Change) {
	switch row.Type {
	case events.TypeJobFinished:
		if row.Text != "" {
			f.run.JobError = row.Text
		}
	case events.TypeStepStarted:
		openStep(&f.run, f.index, row)

		return f.index[stepKey(row)], Change{Opened: true}
	case events.TypeStepFinished, events.TypeStepSkipped:
		if position, closed := closeStep(&f.run, f.index, row, results); closed {
			return position, Change{Closed: true}
		}
	case events.TypeStepOutput:
		attachOutput(&f.run, f.index, row)

		return f.index[stepKey(row)], Change{Other: true}
	case events.TypeStepNote:
		return attachNote(&f.run, f.index, row)
	default:
		return hangTurn(&f.run, f.index, row)
	}

	return 0, Change{}
}

// Steps is every step folded so far, flat and in start order, without
// re-hanging the tree the way View does.
func (f *Folder) Steps() []*Step { return f.run.Steps }

// View is what has been folded so far, with the tree hung and the run row as
// it stands — the row keeps changing under a live fold, and it is read for
// the job error the step template asks each row about. Safe to call after
// every batch: linkTree rebuilds the parent links rather than adding to them.
func (f *Folder) View(run store.RunRow) Transcript {
	f.run.Run = run
	linkTree(&f.run)

	return f.run
}

// attachOutput hangs one of a step's printed outputs on it. The event can
// arrive before the step finishes, so the step is opened if it is not on the
// list yet — the same tolerance closeStep has for a chain-skipped step.
func attachOutput(view *Transcript, index map[string]int, row store.RunEventRow) {
	position, seen := index[stepKey(row)]
	if !seen {
		openStep(view, index, row)
		position = index[stepKey(row)]
	}

	view.Steps[position].Outputs = append(view.Steps[position].Outputs, row.Text)
}

// attachNote hangs a note on the step it names, or on the run when it names none the fold has seen, reporting the row it changed.
func attachNote(view *Transcript, index map[string]int, row store.RunEventRow) (int, Change) {
	note := Note{Level: row.Status, Text: row.Text, At: row.At}

	// Only by id: a note carries no name, so the (index, name) fallback would file every one under whatever step held index 0.
	if position, seen := index[stepKey(row)]; seen && row.StepID != 0 {
		view.Steps[position].Notes = append(view.Steps[position].Notes, note)

		return position, Change{Other: true}
	}

	view.Notes = append(view.Notes, note)

	return 0, Change{}
}

// hangTurn folds agent conversation traffic in; anything unrecognized is ignored rather than rendered, so an event type added later cannot break an older reader.
func hangTurn(view *Transcript, index map[string]int, row store.RunEventRow) (int, Change) {
	if !isAgentTraffic(row.Type) {
		return 0, Change{}
	}

	if position, hung := attachTurn(view, index, row); hung {
		return position, Change{Turns: 1}
	}

	return 0, Change{}
}

// isAgentTraffic reports conversation events, which hang under a step rather
// than being one.
func isAgentTraffic(eventType string) bool {
	switch eventType {
	case events.TypeAgentSystem, events.TypeAgentUser,
		events.TypeAgentText, events.TypeAgentCall, events.TypeAgentResult, events.TypeAgentSubagent:
		return true
	default:
		return false
	}
}

// openStep records a step's start, ignoring a repeat (a step re-entered by a
// to: loop keeps its first position rather than appearing twice).
func openStep(view *Transcript, index map[string]int, row store.RunEventRow) {
	key := stepKey(row)
	if _, seen := index[key]; seen {
		return
	}

	index[key] = len(view.Steps)
	view.Steps = append(view.Steps, &Step{
		ID:       row.StepID,
		ParentID: row.ParentStepID,
		Index:    row.StepIndex,
		Name:     row.StepName,
		Kind:     row.StepKind,
		Started:  row.At,
		FirstSeq: row.Seq,
	})
}

// closeStep records how a step ended, folding in whatever its node recorded,
// and reports the position it wrote to.
func closeStep(
	view *Transcript, index map[string]int, row store.RunEventRow, results map[string]store.NodeRow,
) (int, bool) {
	position, seen := index[stepKey(row)]
	if !seen {
		// A step swallowed by a chain skip never started, so there is no row
		// to close — open one now. Dropping it instead would end the
		// transcript at the cache hit and leave every later step of the plan
		// unaccounted for, which reads as a truncated run rather than a
		// cached one.
		if row.Type != events.TypeStepSkipped {
			return 0, false
		}

		openStep(view, index, row)
		position = index[stepKey(row)]
	}

	step := view.Steps[position]
	step.Status = row.Status
	step.Hash = row.Hash
	step.Duration = time.Duration(row.DurationMS) * time.Millisecond

	// Only when set: a finished step's row carries it, and the events that
	// precede it do not, so an unconditional assignment would blank it.
	if row.Worker != "" {
		step.Worker = row.Worker
	}

	if row.Type == events.TypeStepSkipped {
		step.Reason = row.Text
	} else if step.Failed() {
		step.Error = row.Text
	}

	if node, ok := results[row.Hash]; ok && node.Result != "" {
		step.Result = decodeResult(node.Result)
	}

	return position, true
}

// attachTurn hangs one conversation event on the step it belongs to, and
// reports which step that was — which is not always the one the row names. A
// turn whose step is not in the transcript is dropped rather than inventing a
// step for it — that only happens for a hook or fix conversation, which by
// design records no plan step.
func attachTurn(view *Transcript, index map[string]int, row store.RunEventRow) (int, bool) {
	position, seen := index[stepKey(row)]
	if !seen {
		// A sub-agent's turns carry the CHILD's name, not the plan step's, so
		// fall back to the most recent agent step still running.
		position, seen = lastRunningAgent(view)
		if !seen {
			return 0, false
		}
	}

	view.Steps[position].Turns = append(view.Steps[position].Turns, Turn{
		Type:   row.Type,
		Text:   row.Text,
		Name:   row.Name,
		Detail: row.Detail,
		Depth:  parseDepth(row.Status),
		At:     row.At,
	})

	return position, true
}

// lastRunningAgent finds the newest agent step that has not finished.
func lastRunningAgent(view *Transcript) (int, bool) {
	for i := len(view.Steps) - 1; i >= 0; i-- {
		if view.Steps[i].Kind == "agent" && view.Steps[i].Running() {
			return i, true
		}
	}

	return 0, false
}

// parseDepth reads the "depth:N" marker a nested conversation's events carry.
func parseDepth(status string) int {
	if !strings.HasPrefix(status, "depth:") {
		return 0
	}

	var depth int

	_, err := fmt.Sscanf(status, "depth:%d", &depth)
	if err != nil {
		return 0
	}

	return depth
}

// stepKey identifies one step occurrence within a run.
//
// The minted id when there is one, because it is the only thing that tells a
// try: apart from the step it wraps — those publish the same index and the
// same name, and keying on that pair folded them into a single row that
// reported the wrapper's kind and the wrapped step's nothing.
//
// A run recorded before ids existed has none, and falls back to the pair.
// That run renders exactly as it did then: flat, with the collision it always
// had. Better than one row swallowing the whole plan, which is what keying
// every such step on id 0 would do.
func stepKey(row store.RunEventRow) string {
	if row.StepID != 0 {
		return "#" + strconv.FormatInt(row.StepID, 10)
	}

	return fmt.Sprintf("%d/%s", row.StepIndex, row.StepName)
}

// linkTree hangs each step under the container it named, leaving the steps
// with no container (or one this run never recorded) as roots.
//
// Order is start order throughout: the flat list is appended to as steps
// open, and a child is appended to its parent as it is linked, so a matrix's
// cells appear in the order they began rather than the order they finished.
func linkTree(view *Transcript) {
	// Rebuilt, not appended to: the live stream re-hangs the tree after every
	// flush, and appending would give a container a second copy of each of its
	// children per batch.
	view.Roots = view.Roots[:0]

	for _, step := range view.Steps {
		step.Children = step.Children[:0]
	}

	byID := make(map[int64]*Step, len(view.Steps))

	for _, step := range view.Steps {
		if step.ID != 0 {
			byID[step.ID] = step
		}
	}

	for _, step := range view.Steps {
		parent, nested := byID[step.ParentID]
		// A step cannot contain itself, and a malformed pair must not build a
		// cycle the template would recurse through forever.
		if !nested || parent == step {
			view.Roots = append(view.Roots, step)

			continue
		}

		parent.Children = append(parent.Children, step)
	}
}

// decodeResult decodes a node's stored result JSON, yielding nil rather than
// failing: a result that will not parse is a diagnostic curiosity, not a
// reason for the page not to render.
func decodeResult(raw string) map[string]any {
	var result map[string]any

	err := json.Unmarshal([]byte(raw), &result)
	if err != nil {
		return nil
	}

	return result
}

// Slug renders a step name as a URL fragment: lowercase, with every run of
// non-alphanumerics collapsed to a single dash. An across: cell named
// review[security] becomes review-security, so a step is linkable by name
// rather than by position alone.
func Slug(name string) string {
	var out strings.Builder

	dashed := false

	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)

			dashed = false

			continue
		}

		if !dashed && out.Len() > 0 {
			out.WriteByte('-')

			dashed = true
		}
	}

	return strings.TrimSuffix(out.String(), "-")
}
