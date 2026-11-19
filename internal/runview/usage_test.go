package runview

import (
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// The job's usage report is the terminal's; a page draws each number on its own row. A run recorded before the note was named is recognised by its words, and a step's note that happens to read alike is not.
func TestTheUsageReportIsNotARunNote(t *testing.T) {
	t.Parallel()

	view := Build(store.RunRow{ID: "run"}, []store.RunEventRow{
		{Seq: 1, Type: events.TypeStepNote, Status: events.NoteInfo, Name: events.NoteUsage, Text: "usage: 10 tokens across 1 agent step(s)"},
		{Seq: 2, Type: events.TypeStepNote, Status: events.NoteInfo, Text: "usage: 10 tokens across 1 agent step(s)\n  review 10"},
		{Seq: 3, Type: events.TypeStepNote, Status: events.NoteInfo, Text: "worker released"},
	}, nil)

	if len(view.Notes) != 1 || view.Notes[0].Text != "worker released" {
		t.Errorf("run notes = %+v, want only the release", view.Notes)
	}
}
