package web

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// TestTranscriptStepsCarryStatusGlyph: a step row's outcome must be readable
// without its color — the 3px rail tick was the only signal, and it is
// invisible to a screen reader and ambiguous to anyone colorblind.
func TestTranscriptStepsCarryStatusGlyph(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-glyph", "build", "/tmp/ws")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-glyph", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "repo", StepKind: "get"},
		{Type: events.TypeStepSkipped, StepIndex: 0, StepName: "repo", StepKind: "get", Status: "skipped"},
		{Type: events.TypeStepStarted, StepIndex: 1, StepName: "compile", StepKind: "task"},
		{Type: events.TypeStepFinished, StepIndex: 1, StepName: "compile", StepKind: "task", Status: "succeeded"},
		{Type: events.TypeStepStarted, StepIndex: 2, StepName: "ship", StepKind: "task"},
		{Type: events.TypeStepFinished, StepIndex: 2, StepName: "ship", StepKind: "task", Status: "failed", Text: "exit 1"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-glyph", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-glyph")

	for _, want := range []string{
		`class="stmark" role="img" aria-label="passed"`,
		`class="stmark" role="img" aria-label="failed"`,
		`class="stmark" role="img" aria-label="skipped"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("transcript missing %q", want)
		}
	}
}

// TestApprovalsUseSharedTimeAndStatusVocabulary: the cards showed raw stored
// strings next to pages that say "4s ago", and their outcome stamp lost the
// glyph every other status badge carries.
func TestApprovalsUseSharedTimeAndStatusVocabulary(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	id, err := pipeline.Store.RequestApproval(ctx, "deploy", "ship it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	err = pipeline.Store.DecideApproval(ctx, id, "approved", "test", "fine")
	if err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}

	_, body := get(t, server, "/p/demo/approvals")

	if !strings.Contains(body, `<time data-ago=`) {
		t.Error("approvals page shows raw timestamps, not relative time")
	}

	if !strings.Contains(body, `class="stamp st st-passed"`) {
		t.Error("approval outcome stamp is missing the shared .st badge class")
	}
}

// TestQuestionsUseSharedTimeAndStatusVocabulary — same contract as approvals.
func TestQuestionsUseSharedTimeAndStatusVocabulary(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-q", "deploy", "/tmp/ws")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	question, _, err := pipeline.Store.AskQuestion(ctx, store.Question{
		RunID: "run-q", JobName: "deploy", AgentName: "reviewer", Question: "which region?",
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	err = pipeline.Store.AnswerQuestion(ctx, question.ID, "us-east-2", "test")
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}

	_, body := get(t, server, "/p/demo/questions")

	if !strings.Contains(body, `<time data-ago=`) {
		t.Error("questions page shows raw timestamps, not relative time")
	}

	if !strings.Contains(body, `class="stamp st st-passed"`) {
		t.Error("question outcome stamp is missing the shared .st badge class")
	}
}

// TestAgoTextFallsBackToTheRawStamp: a stamp that will not parse renders
// as-is rather than hiding the record.
func TestAgoTextFallsBackToTheRawStamp(t *testing.T) {
	t.Parallel()

	if got := string(agoText("not-a-time")); got != "not-a-time" {
		t.Errorf("agoText fallback = %q, want the raw stamp", got)
	}

	if got := string(agoText("2026-08-31T00:00:00.000000000Z")); !strings.Contains(got, "<time data-ago=") {
		t.Errorf("agoText did not render a parseable stamp relatively: %q", got)
	}
}

// TestJobPageSaysPassedNotGreen: one state, one word. Tables say "passed";
// the job page said "green" for the same fact.
func TestJobPageSaysPassedNotGreen(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, body := get(t, server, "/p/demo/jobs/deploy")

	if strings.Contains(body, "green") {
		t.Error(`job page still says "green" for a passed state`)
	}

	if !strings.Contains(body, "must have passed for") {
		t.Error("deploy page does not phrase its upstream constraint in the shared vocabulary")
	}
}
