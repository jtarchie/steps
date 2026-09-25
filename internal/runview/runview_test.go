package runview

import (
	"testing"

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
