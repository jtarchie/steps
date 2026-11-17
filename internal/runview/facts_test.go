package runview

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

func factRows() []store.RunEventRow {
	return []store.RunEventRow{
		{Seq: 1, Type: events.TypeStepStarted, StepID: 1, StepIndex: 0, StepName: "pr", StepKind: "get"},
		{Seq: 2, Type: events.TypeStepNote, StepID: 1, Status: events.NoteInfo, Name: events.NoteFact, Detail: "url", Text: "https://x/pull/1"},
		{Seq: 3, Type: events.TypeStepFinished, StepID: 1, StepIndex: 0, StepName: "pr", StepKind: "get", Status: "succeeded"},
		{Seq: 4, Type: events.TypeStepStarted, StepID: 2, StepIndex: 1, StepName: "review", StepKind: "put"},
		{Seq: 5, Type: events.TypeStepNote, StepID: 2, Status: events.NoteInfo, Name: events.NotePublished, Detail: "url", Text: "https://x/pull/1#c2"},
		{Seq: 6, Type: events.TypeStepNote, StepID: 2, Status: events.NoteInfo, Name: events.NotePublished, Detail: "script", Text: "javascript:alert(1)"},
		{Seq: 7, Type: events.TypeStepFinished, StepID: 2, StepIndex: 1, StepName: "review", StepKind: "put", Status: "succeeded"},
		{Seq: 8, Type: events.TypeStepNote, Status: events.NoteInfo, Name: events.NotePublished, Detail: "orphan", Text: "https://x/o"},
	}
}

// TestAFactIsFoldedOntoItsStepAndAPutsLinkIsPublished: a fact is the step's, apart from its notes; only a put's http(s) values are what the run published; and a fact no step claims is still said, as a run note naming the fact.
func TestAFactIsFoldedOntoItsStepAndAPutsLinkIsPublished(t *testing.T) {
	t.Parallel()

	view := Build(store.RunRow{}, factRows(), nil)

	if got := fmt.Sprint(view.Steps[0].Facts); got != "[{url https://x/pull/1 false}]" {
		t.Errorf("the get's facts = %s", got)
	}

	if len(view.Steps[0].Notes) != 0 || !view.Steps[0].HasBody() {
		t.Errorf("a fact is drawn as a fact, and gives its step a body: notes %v", view.Steps[0].Notes)
	}

	published := view.Published()
	if len(published) != 1 || published[0].Step != "review" || published[0].Fact.Link() != "https://x/pull/1#c2" {
		t.Errorf("published = %+v, want only the put's link", published)
	}

	if len(view.Notes) != 1 || view.Notes[0].Text != "orphan: https://x/o" {
		t.Errorf("run notes = %+v, want the unclaimed fact said with its name", view.Notes)
	}
}

func TestPlainPrintsAFactUnderItsStepAsItArrives(t *testing.T) {
	t.Parallel()

	var out strings.Builder

	plain := Plain(&out)
	for _, row := range factRows()[:3] {
		plain(EventOf(row, "j"))
	}

	if got := out.String(); got != "get: pr\n  url: https://x/pull/1\n" {
		t.Errorf("plain = %q", got)
	}
}

// TestLiveDrawsFactsUnderTheFinishedStepAndEndsOnWhatWasPublished: the live view holds a step's facts until its line is drawn, so they sit under it rather than wherever they arrived, and the summary lists what the run's puts published.
func TestLiveDrawsFactsUnderTheFinishedStepAndEndsOnWhatWasPublished(t *testing.T) {
	t.Parallel()

	var out strings.Builder

	live := NewLive(&out)
	for _, row := range append(factRows()[:7], store.RunEventRow{Seq: 9, Type: events.TypeJobFinished, Status: "succeeded", StepIndex: -1}) {
		live.Event(EventOf(row, "j"))
	}

	live.Stop()

	got := out.String()

	for _, want := range []string{
		"✓ get pr · 0.0s\n    url: https://x/pull/1\n",
		"✓ put review · 0.0s\n    url:    https://x/pull/1#c2\n    script: javascript:alert(1)\n",
		"Published\n  review  https://x/pull/1#c2\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("live view is missing %q:\n%s", want, got)
		}
	}
}
