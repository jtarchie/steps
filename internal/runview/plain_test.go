package runview

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
)

// TestPlainPrintsOneLinePerThingThatHappened pins the plain terminal's shape: a start names the kind and the step, a skip says why, a note is its own text with a warning saying so in words, and the rest (a finish, output, a conversation) prints nothing, because the output already streamed and the job's error is the CLI's to print.
func TestPlainPrintsOneLinePerThingThatHappened(t *testing.T) {
	t.Parallel()

	var out strings.Builder

	plain := Plain(&out)

	for _, event := range []events.Event{
		{Type: events.TypeJobStarted},
		{Type: events.TypeStepStarted, StepKind: "task", StepName: "compile"},
		{Type: events.TypeStepOutput, StepName: "compile", Text: "already streamed"},
		{Type: events.TypeStepFinished, StepName: "compile", Status: "succeeded"},
		{Type: events.TypeStepStarted, StepKind: "do"},
		{Type: events.TypeStepSkipped, StepName: "test", Text: "when: guard was false"},
		{Type: events.TypeStepSkipped, StepName: "lint"},
		{Type: events.TypeStepNote, Status: events.NoteInfo, Text: "get: repo (version: map[ref:abc])"},
		{Type: events.TypeStepNote, Status: events.NoteWarn, Text: "could not refresh repo"},
		{Type: events.TypeAgentText, Text: "thinking"},
		{Type: events.TypeJobFinished, Status: "failed", Text: "boom"},
	} {
		plain(event)
	}

	want := strings.Join([]string{
		"task: compile",
		"do",
		"skip: test (when: guard was false)",
		"skip: lint",
		"get: repo (version: map[ref:abc])",
		"warning: could not refresh repo",
	}, "\n") + "\n"

	if got := out.String(); got != want {
		t.Errorf("plain output:\n%s\nwant:\n%s", got, want)
	}
}
