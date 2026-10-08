package web

import (
	"context"
	"regexp"
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

	err := pipeline.Store.StartRun(ctx, "run-glyph", "build", "/tmp/ws", "")
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

	err := pipeline.Store.StartRun(ctx, "run-q", "deploy", "/tmp/ws", "")
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

	_, body := get(t, server, "/p/demo/jobs/deploy/detail")

	if strings.Contains(body, "green") {
		t.Error(`job page still says "green" for a passed state`)
	}

	if !strings.Contains(body, "must have passed for") {
		t.Error("deploy page does not phrase its upstream constraint in the shared vocabulary")
	}
}

// TestAHeldJobIsSaidOnTheJobsBoardAndItsPage: held is a job's state, so it lives with the job — it used to be a card on the resources page, where nobody looks for a job. The read-only hint names the EXACT command: it once said `steps web`, which on a machine sharing the state file is the forbidden second daemon.
func TestAHeldJobIsSaidOnTheJobsBoardAndItsPage(t *testing.T) {
	t.Parallel()

	// testPipeline passes runner == nil, which IS the read-only deployment.
	server, pipeline := testPipeline(t)
	ctx := context.Background()

	held, _, err := pipeline.Store.RecordJobOutcome(ctx, "build", false, 1)
	if err != nil || !held {
		t.Fatalf("RecordJobOutcome = %v, %v: the job was not held", held, err)
	}

	_, board := get(t, server, "/p/demo")

	build := dagNodeMarkup(t, dagSVG(t, board), "job:build")
	if !strings.Contains(build, "· held</text>") || !regexp.MustCompile(`<title>[^<]*held after 1 failure, [^<]* ago[^<]*</title>`).MatchString(build) {
		t.Errorf("the jobs board does not say the job is held, since when:\n%s", build)
	}

	_, resources := get(t, server, "/p/demo/resources")
	if strings.Contains(resources, "Circuit breaker") {
		t.Error("the resources page still carries the job breaker")
	}

	_, page := get(t, server, "/p/demo/jobs/build/detail")

	// The pipeline's NAME rather than a path: a served pipeline no longer has a file on this machine, and the name is what every verb takes.
	if !strings.Contains(page, "<code>steps jobs release build -p demo</code>") {
		t.Error("read-only job page does not name the release command for the held job and its pipeline")
	}
}

// TestJobsBoardSaysNeverRan: the list view said "never run" while the graph
// said "never ran" — two spellings of one state on one board. The list is
// gone; the spelling it lost to stays.
func TestJobsBoardSaysNeverRan(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, body := get(t, server, "/p/demo")

	if strings.Contains(body, ">never run<") {
		t.Error(`jobs board still spells the state "never run" somewhere`)
	}

	if strings.Count(body, "never ran") < 2 {
		t.Error("expected both of the board's jobs to say \"never ran\"")
	}
}

// TestEveryStateHasOneWordGlyphAndColour: one vocabulary across every page (.design/run-actions-and-states). A human stop is not a failure, so aborted and a paused pipeline are not drawn red with ✗; queued is not a verdict, so it is not coloured like running; errored is the machinery breaking, so it does not share failed's glyph.
func TestEveryStateHasOneWordGlyphAndColour(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)
	_, css := get(t, server, "/static/app.css")

	for _, tc := range []struct{ stored, word, glyph, colour string }{
		{"pending", "queued", "○", "--faint"},
		{"running", "running", "◐", "--yellow"},
		{"succeeded", "passed", "✓", "--green"},
		{"failed", "failed", "✗", "--red"},
		{"errored", "errored", "!", "--red"},
		{"aborted", "aborted", "■", "--dim"},
		{"paused", "paused", "⏸", "--blue"},
		{"held", "held", "⊘", "--red"},
	} {
		if got := statusWord(tc.stored); got != tc.word {
			t.Errorf("statusWord(%q) = %q, want %q", tc.stored, got, tc.word)
		}

		if !cssRule(css, ".st.st-"+tc.word+"::before", `content: "`+tc.glyph+` "`) {
			t.Errorf("%s: no .st::before rule drawing %q", tc.word, tc.glyph)
		}

		if !cssRule(css, ".st-"+tc.word, "color: var("+tc.colour+")") {
			t.Errorf("%s: not coloured %s", tc.word, tc.colour)
		}
	}
}

// TestTheOverviewSaysPausedInBlueAndNothingOtherwise: "running" beside a
// pipeline read as a run in progress, and a paused one was drawn as a
// failure. Then "active" on every healthy row was a column saying nothing,
// so a healthy row now carries no state word or badge at all.
func TestTheOverviewSaysPausedInBlueAndNothingOtherwise(t *testing.T) {
	t.Parallel()

	server, pipelines := testPipelines(t, "held", "red", "live")

	for _, pipeline := range pipelines[:2] {
		err := pipeline.Store.Pause(t.Context())
		if err != nil {
			t.Fatalf("Pause: %v", err)
		}
	}

	err := pipelines[1].Store.StartRun(t.Context(), "run-red", "red-job", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = pipelines[1].Store.FinishRun(t.Context(), "run-red", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, body := get(t, server, "/")

	// The mark already draws ⏸ when pause is the loudest thing about the
	// pipeline; a chip beside it said the same thing twice.
	held := between(t, pipelineRow(t, body, "held"), `<td class="pname">`, "</td>")
	if !strings.Contains(held, `<span class="st-paused">⏸</span>`) || strings.Contains(held, `class="st st-paused"`) {
		t.Errorf("a paused pipeline's name cell should say paused once, by its mark:\n%s", held)
	}

	// A failure outranks the pause in the mark, so the chip is then the only
	// place the row says it is stopped.
	if !strings.Contains(pipelineRow(t, body, "red"), `<span class="st st-paused">paused</span>`) {
		t.Error("a paused pipeline whose mark is a failure does not say it is paused")
	}

	live := pipelineRow(t, body, "live")
	name := between(t, live, `<td class="pname">`, "</td>")

	for _, noise := range []string{"active", "running", `class="st `, `class="counts"`} {
		if strings.Contains(name, noise) {
			t.Errorf("a healthy pipeline's name cell says %q:\n%s", noise, name)
		}
	}
}

// cssRule reports whether some rule whose selector list names selector declares decl.
func cssRule(css, selector, decl string) bool {
	for _, rule := range strings.Split(css, "}") {
		head, body, ok := strings.Cut(rule, "{")
		if !ok || !strings.Contains(body, decl) {
			continue
		}

		for _, sel := range strings.Split(head, ",") {
			if strings.TrimSpace(sel[strings.LastIndex(sel, "\n")+1:]) == selector {
				return true
			}
		}
	}

	return false
}

// TestAFocusedGraphBoxIsDrawnFocused: the board is a drawing a keyboard
// tabs through, and an outline on an svg link is not reliably painted, so a
// focused box carries the page's focus color on its own border.
func TestAFocusedGraphBoxIsDrawnFocused(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)
	_, css := get(t, server, "/static/app.css")

	for _, selector := range []string{".dagnode:focus-visible .dagbox", ".dagres:focus-visible rect"} {
		if !cssRule(css, selector, "stroke: var(--yellow)") {
			t.Errorf("%s is not drawn in the focus color", selector)
		}
	}
}
