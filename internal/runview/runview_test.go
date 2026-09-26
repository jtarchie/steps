package runview

import (
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// TestSlugify covers the anchor-name rules directly, including the shapes
// across: cells and hook labels actually produce.
func TestSlugify(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
		{"compile", "compile"},
		{"review[security]", "review-security"},
		{"Deploy To Prod", "deploy-to-prod"},
		{"unit-tests", "unit-tests"},
		{"a//b", "a-b"},
		{"...", ""},
		{"", ""},
	} {
		if got := Slug(tc.in); got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRunViewCarriesTheWorker pins that the post-hoc view says where a placed
// step ran, and says nothing for one that ran here.
func TestRunViewCarriesTheWorker(t *testing.T) {
	t.Parallel()

	rows := []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "here", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "here", StepID: 1, Status: "succeeded"},
		{Type: events.TypeStepStarted, StepIndex: 1, StepName: "there", StepID: 2},
		{Type: events.TypeStepFinished, StepIndex: 1, StepName: "there", StepID: 2, Status: "failed",
			Worker: "gpu (ssh://jt@box)"},
	}

	view := Build(store.RunRow{ID: "R1"}, rows, nil)

	byName := map[string]*Step{}
	for _, step := range view.Steps {
		byName[step.Name] = step
	}

	if got := byName["there"].Worker; got != "gpu (ssh://jt@box)" {
		t.Errorf("placed step worker = %q, want the machine it ran on", got)
	}

	if got := byName["here"].Worker; got != "" {
		t.Errorf("local step worker = %q, want nothing — naming every local step would bury the ones that left", got)
	}
}

// TestAnswerIsNotPrintedTwice covers the dedup between the last model text and
// the labeled answer — including the shape that used to slip through, where the
// model emitted text AND a tool call in one message, so the text is not the
// trailing turn.
func TestAnswerIsNotPrintedTwice(t *testing.T) {
	t.Parallel()

	const answer = "Two SKUs need restocking."

	step := Step{
		Result: map[string]any{"response": answer},
		Turns: []Turn{
			{Type: events.TypeAgentText, Text: "Reading the inventory first."},
			{Type: events.TypeAgentText, Text: answer},
			// Recorded after the answer: the same assistant message carried a
			// tool call, so the result lands last.
			{Type: events.TypeAgentCall, Name: "read_file"},
			{Type: events.TypeAgentResult, Name: "read_file"},
		},
	}

	kept := step.Conversation()
	if len(kept) != 3 {
		t.Fatalf("Conversation kept %d turns, want 3: %+v", len(kept), kept)
	}

	for _, turn := range kept {
		if turn.Text == answer {
			t.Error("the answer is still in the conversation as well as under `answer`")
		}
	}

	// The mid-conversation commentary is not the answer and must survive.
	if kept[0].Text != "Reading the inventory first." {
		t.Errorf("dropped the wrong turn: %+v", kept)
	}

	// A response the model never said as text leaves every turn alone.
	other := Step{Result: map[string]any{"response": "different"}, Turns: step.Turns}
	if len(other.Conversation()) != len(step.Turns) {
		t.Error("a non-matching response dropped a turn anyway")
	}
}

// TestNotesHangOnTheirStep covers where a note lands: under the step whose id
// it carries, or on the run itself when it names no step the fold knows —
// never dropped, because a note is the only record of what it says.
func TestNotesHangOnTheirStep(t *testing.T) {
	t.Parallel()

	rows := []store.RunEventRow{
		{Seq: 1, Type: events.TypeStepStarted, StepName: "build", StepID: 1},
		{Seq: 2, Type: events.TypeStepNote, StepID: 1, Status: events.NoteInfo, Text: "pulled alpine"},
		{Seq: 3, Type: events.TypeStepNote, StepIndex: -1, Status: events.NoteWarn, Text: "budget at 90%"},
		{Seq: 4, Type: events.TypeStepNote, StepID: 9, Status: events.NoteInfo, Text: "orphan"},
		{Seq: 5, Type: events.TypeStepFinished, StepName: "build", StepID: 1, Status: "succeeded"},
	}

	folder := NewFolder()
	changes := folder.Add(rows, nil)
	view := folder.View(store.RunRow{ID: "R"})

	if len(view.Steps) != 1 {
		t.Fatalf("a note opened a step: %d steps", len(view.Steps))
	}

	build := view.Steps[0]
	if len(build.Notes) != 1 || build.Notes[0].Text != "pulled alpine" || build.Notes[0].Level != events.NoteInfo {
		t.Errorf("build notes = %+v, want the one note it carried", build.Notes)
	}

	if !build.HasBody() {
		t.Error("a step whose only content is a note has no body, so the page cannot show it")
	}

	if len(view.Notes) != 2 || view.Notes[0].Text != "budget at 90%" || view.Notes[1].Text != "orphan" {
		t.Errorf("run notes = %+v, want the job-level and the unknown-step note in order", view.Notes)
	}

	if !changes[build.Key()].Other {
		t.Error("a note did not mark its row changed, so the live stream would not redraw it")
	}
}

// TestCompactionHangsOnItsStep covers the compaction marker and badge: an
// agent_compaction row folds in as a turn (an unlisted type is silently
// dropped), and the count reads from a stored result's float64 as well as an
// in-process int.
func TestCompactionHangsOnItsStep(t *testing.T) {
	t.Parallel()

	rows := []store.RunEventRow{
		{Seq: 1, Type: events.TypeStepStarted, StepName: "review", StepID: 1, StepKind: "agent"},
		{Seq: 2, Type: events.TypeAgentCompaction, StepName: "review", StepID: 1, Name: "compacted: 3 messages summarized", Text: "the summary"},
	}

	folder := NewFolder()
	changes := folder.Add(rows, nil)
	view := folder.View(store.RunRow{ID: "R"})

	turns := view.Steps[0].Turns
	if len(turns) != 1 || !turns[0].IsCompaction() || turns[0].Text != "the summary" {
		t.Fatalf("turns = %+v, want the compaction marker carrying its summary", turns)
	}

	if changes[view.Steps[0].Key()].Turns != 1 {
		t.Errorf("change = %+v, want one turn", changes[view.Steps[0].Key()])
	}

	for _, tc := range []struct {
		result  map[string]any
		want    int
		stalled bool
	}{
		{result: map[string]any{"compactions": float64(2), "compaction_stalled": true}, want: 2, stalled: true},
		{result: map[string]any{"compactions": 3}, want: 3},
		{result: map[string]any{"response": "ok"}},
		{},
	} {
		step := Step{Result: tc.result}
		if got := step.Compactions(); got != tc.want {
			t.Errorf("Compactions(%v) = %d, want %d", tc.result, got, tc.want)
		}

		if got := step.CompactionStalled(); got != tc.stalled {
			t.Errorf("CompactionStalled(%v) = %v, want %v", tc.result, got, tc.stalled)
		}
	}
}

// unclosed is a transcript with rows nothing closed: an orphan output (1), a
// container (2) holding a started child (3), and one step that finished (4).
func unclosed(t0 time.Time) []store.RunEventRow {
	return []store.RunEventRow{
		{Seq: 1, Type: events.TypeStepOutput, StepID: 1, StepName: "hook", StepKind: "task", Text: "hook said", At: t0},
		{Seq: 2, Type: events.TypeStepStarted, StepID: 2, StepIndex: 1, StepName: "block", StepKind: "do", At: t0},
		{Seq: 3, Type: events.TypeStepStarted, StepID: 3, ParentStepID: 2, StepIndex: 2, StepName: "inner", StepKind: "task", At: t0},
		{Seq: 4, Type: events.TypeStepStarted, StepID: 4, StepIndex: 3, StepName: "done", StepKind: "task", At: t0},
		{Seq: 5, Type: events.TypeStepFinished, StepID: 4, StepIndex: 3, StepName: "done", StepKind: "task", Status: "succeeded", DurationMS: 5},
	}
}

func stepsByID(view Transcript) map[int64]*Step {
	out := map[int64]*Step{}
	for _, step := range view.Steps {
		out[step.ID] = step
	}

	return out
}

// TestAStepThatNeverReportedStopsRunningWhenTheRunEnds covers the guard for a
// row the fold opened and nothing closed (#169): on an ended run it is
// unreported, never running and never the run's outcome.
func TestAStepThatNeverReportedStopsRunningWhenTheRunEnds(t *testing.T) {
	t.Parallel()

	for _, status := range []string{"succeeded", "failed", "errored", "aborted"} {
		assertSettled(t, status, stepsByID(Build(store.RunRow{Status: status}, unclosed(time.Now()), nil)))
	}
}

func assertSettled(t *testing.T, status string, steps map[int64]*Step) {
	t.Helper()

	for id, step := range steps {
		// Step 4 is the one that finished; every other row never closed.
		closed := id == 4
		if step.Running() || step.Active() || step.Unreported() == closed || step.Failed() {
			t.Errorf("%s run: step %d = %+v, want unreported %v and neither running nor failed", status, id, step, !closed)
		}
	}

	if steps[4].Status != "succeeded" {
		t.Errorf("%s run: a finished step was touched: %+v", status, steps[4])
	}

	if tally := steps[2].Rollup(); tally.Unreported != 1 || tally.Passed != 0 || tally.Running != 0 {
		t.Errorf("%s run: rollup = %+v, want the one child unreported", status, tally)
	}
}

// TestAStepThatNeverReportedKeepsRunningMidRun is the live half: "" is the
// terminal live view's zero RunRow, and pending stands for any status not
// known to be final — both must keep an open step running.
func TestAStepThatNeverReportedKeepsRunningMidRun(t *testing.T) {
	t.Parallel()

	for _, status := range []string{"running", "", "pending"} {
		steps := stepsByID(Build(store.RunRow{Status: status}, unclosed(time.Now()), nil))

		for _, id := range []int64{1, 3} {
			if !steps[id].Running() || !steps[id].Active() {
				t.Errorf("%q run: step %d stopped running mid-run", status, id)
			}
		}

		for id, step := range steps {
			if step.Unreported() {
				t.Errorf("%q run: step %d unreported mid-run", status, id)
			}
		}
	}
}

// TestAnUnreportedStepLastsUntilTheRunEnded pins the duration, including the
// two cases where one end is unknown or the clocks disagree.
func TestAnUnreportedStepLastsUntilTheRunEnded(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name     string
		finished time.Time
		want     time.Duration
	}{
		{"finish known", t0.Add(90 * time.Second), 90 * time.Second},
		{"finish unknown", time.Time{}, 0},
		{"finish before start", t0.Add(-time.Second), 0},
	} {
		steps := stepsByID(Build(store.RunRow{Status: "failed", FinishedAt: tc.finished}, unclosed(t0), nil))
		if got := steps[1].Duration; got != tc.want {
			t.Errorf("%s: duration = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestALateCloseBeatsTheGuard covers the event sink draining after the run
// row went terminal: the flag is recomputed per View, so the real outcome
// wins on the next one.
func TestALateCloseBeatsTheGuard(t *testing.T) {
	t.Parallel()

	folder := NewFolder()
	folder.Add(unclosed(time.Now()), nil)
	folder.View(store.RunRow{Status: "failed"})
	folder.Add([]store.RunEventRow{
		{Seq: 6, Type: events.TypeStepFinished, StepID: 1, StepName: "hook", StepKind: "task", Status: "succeeded"},
	}, nil)

	if late := stepsByID(folder.View(store.RunRow{Status: "failed"}))[1]; late.Unreported() || late.Status != "succeeded" {
		t.Errorf("a close landing after the run ended lost to the guard: %+v", late)
	}
}

// TestTheFoldDoesNotReadTheLastView pins that routing a sub-agent's turn to
// its step is a function of the events alone: a Folder once Viewed against
// an ended row must still hang the turn on the open agent step.
func TestTheFoldDoesNotReadTheLastView(t *testing.T) {
	t.Parallel()

	folder := NewFolder()
	folder.Add([]store.RunEventRow{
		{Seq: 1, Type: events.TypeStepStarted, StepID: 1, StepName: "review", StepKind: "agent"},
	}, nil)
	folder.View(store.RunRow{Status: "failed"})
	folder.Add([]store.RunEventRow{
		{Seq: 2, Type: events.TypeAgentText, StepName: "sub-agent", Text: "looking"},
	}, nil)

	if turns := folder.Steps()[0].Turns; len(turns) != 1 {
		t.Errorf("sub-agent turn hung on %d turns, want 1 on the open agent step", len(turns))
	}
}
