package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// stubRunner makes a server writable. The questions page needs one for the
// same reason the approvals page does: the answer control is withheld from a
// read-only server, and nil is what read-only means here.
type stubRunner struct{}

func (stubRunner) Enqueue(context.Context, *Pipeline, string, string, bool) (int64, error) {
	return 1, nil
}

// askOne records a pending question against a live run, the way an agent step
// would.
func askOne(t *testing.T, pipeline *Pipeline) store.Question {
	t.Helper()

	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-1", "build", "/tmp/ws")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	question, _, err := pipeline.Store.AskQuestion(ctx, store.Question{
		RunID: "run-1", JobName: "build", AgentName: "writer",
		Question: "Is this release a major or a minor bump?",
		Options:  []string{"major", "minor"},
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	return question
}

// post submits a form and returns the status.
func post(t *testing.T, server *Server, target string, form map[string]string) int {
	t.Helper()

	values := make([]string, 0, len(form))
	for key, value := range form {
		values = append(values, key+"="+strings.ReplaceAll(value, " ", "+"))
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(strings.Join(values, "&")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Code
}

// TestQuestionsPageShowsWhatWasAskedAndOffered: a page that says what was
// chosen without saying what was on offer cannot be answered in one keystroke,
// which is most of why the options exist.
func TestQuestionsPageShowsWhatWasAskedAndOffered(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)
	askOne(t, pipeline)

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	code, body := get(t, server, "/p/demo/questions")
	if code != http.StatusOK {
		t.Fatalf("GET /p/demo/questions = %d", code)
	}

	for _, want := range []string{"major or a minor bump", "writer", "build", `value="minor"`} {
		if !strings.Contains(body, want) {
			t.Errorf("questions page missing %q", want)
		}
	}
}

// TestQuestionsPageAnswersThroughTheSameRow: the browser is one more answering
// channel, not a second mechanism. What it writes is what `steps answer`
// writes and what a parked step is polling.
func TestQuestionsPageAnswersThroughTheSameRow(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	question := askOne(t, pipeline)

	if code := post(t, server, "/p/demo/questions/1", map[string]string{"answer": "minor"}); code != http.StatusSeeOther {
		t.Fatalf("POST answer = %d, want a redirect", code)
	}

	answered, err := pipeline.Store.QuestionStatus(context.Background(), question.ID)
	if err != nil {
		t.Fatalf("QuestionStatus: %v", err)
	}

	if answered.Status != "answered" || answered.Answer != "minor" || answered.AnsweredBy != "web" {
		t.Errorf("question row = %+v, want answered/minor/web", answered)
	}

	// Free text arrives under its own field so an option button can win over
	// something already typed; both have to reach the row.
	second, _, err := pipeline.Store.AskQuestion(context.Background(), store.Question{
		RunID: "run-1", JobName: "build", AgentName: "writer", Question: "Which environment?",
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	post(t, server, "/p/demo/questions/2", map[string]string{"answer_text": "staging"})

	typed, err := pipeline.Store.QuestionStatus(context.Background(), second.ID)
	if err != nil {
		t.Fatalf("QuestionStatus: %v", err)
	}

	if typed.Answer != "staging" {
		t.Errorf("free-text answer = %q, want staging", typed.Answer)
	}
}

// TestQuestionsPageWithholdsTheControlWhenReadOnly: --read-only withholds the
// answer control the same way it withholds approve/reject, and says how to
// answer instead rather than presenting a form that would 403.
func TestQuestionsPageWithholdsTheControlWhenReadOnly(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	askOne(t, pipeline)

	_, body := get(t, server, "/p/demo/questions")

	if strings.Contains(body, "<form") {
		t.Error("a read-only server offered an answer form")
	}

	if !strings.Contains(body, "steps answer") {
		t.Error("a read-only server did not say how to answer instead")
	}

	if code := post(t, server, "/p/demo/questions/1", map[string]string{"answer": "minor"}); code != http.StatusForbidden {
		t.Errorf("POST to a read-only server = %d, want 403", code)
	}
}

// TestQuestionsBadgeCountsOnlyWhatIsWaiting: the nav badge is the whole reason
// somebody looks at this page, so it must not keep counting an answered one.
func TestQuestionsBadgeCountsOnlyWhatIsWaiting(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	question := askOne(t, pipeline)

	_, body := get(t, server, "/p/demo/questions")
	if !strings.Contains(body, `questions<span class="badge">●1`) {
		t.Error("a pending question did not raise the nav badge")
	}

	err := pipeline.Store.AnswerQuestion(context.Background(), question.ID, "minor", "jtarchie")
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}

	_, answered := get(t, server, "/p/demo/questions")
	if strings.Contains(answered, `questions<span class="badge">`) {
		t.Error("an answered question is still counted as waiting")
	}
}

// TestAnswerQuestionRefusesWhatItCannotRecord covers the two ways the form can
// arrive unusable. Both are 400s rather than silent no-ops: a question left
// pending because a submission went nowhere is a step still waiting.
func TestAnswerQuestionRefusesWhatItCannotRecord(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	askOne(t, pipeline)

	if code := post(t, server, "/p/demo/questions/not-a-number", map[string]string{"answer": "minor"}); code != http.StatusBadRequest {
		t.Errorf("POST to a non-numeric id = %d, want 400", code)
	}

	if code := post(t, server, "/p/demo/questions/1", map[string]string{"answer_text": "   "}); code != http.StatusBadRequest {
		t.Errorf("POST with a blank answer = %d, want 400", code)
	}

	still, err := pipeline.Store.QuestionStatus(context.Background(), 1)
	if err != nil {
		t.Fatalf("QuestionStatus: %v", err)
	}

	if still.Status != "pending" {
		t.Errorf("a refused submission resolved the question anyway: %q", still.Status)
	}
}

// TestAnswerQuestionShowsARefusalRatherThanRedirecting: a resubmission and a
// REFUSED answer are different failures, and only one of them is a no-op. An
// options-fence rejection redirected to a page still listing the question as
// pending with no message anywhere, so the person retypes the same thing or
// concludes the step is stuck — while the agent is still waiting on them.
func TestAnswerQuestionShowsARefusalRatherThanRedirecting(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	_, pipeline := testPipeline(t)

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = pipeline.Store.StartRun(ctx, "run-1", "build", "/tmp/ws")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	fenced, _, err := pipeline.Store.AskQuestion(ctx, store.Question{
		RunID: "run-1", JobName: "build", AgentName: "writer",
		Question: "Which environment?", Options: []string{"staging", "prod"},
		OptionsRequired: true,
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	if code := post(t, server, "/p/demo/questions/1", map[string]string{"answer_text": "canary"}); code != http.StatusBadRequest {
		t.Errorf("POST of an off-list answer = %d, want 400 so the answerer sees the refusal", code)
	}

	// A resubmission is still a no-op, not an error: the row already has its
	// answer and settling on it is the right posture.
	err = pipeline.Store.AnswerQuestion(ctx, fenced.ID, "prod", "jtarchie")
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}

	if code := post(t, server, "/p/demo/questions/1", map[string]string{"answer": "staging"}); code != http.StatusSeeOther {
		t.Errorf("POST to an already-answered question = %d, want a redirect", code)
	}
}

// TestQuestionsFormPutsFreeTextFirst: pressing Enter in a text input submits
// the form with its DEFAULT button — the first submit button in tree order.
// With the option buttons first, typing an answer and pressing Enter silently
// submitted the first OPTION instead of what was typed.
func TestQuestionsFormPutsFreeTextFirst(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)
	askOne(t, pipeline)

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, body := get(t, server, "/p/demo/questions")

	_, form, found := strings.Cut(body, "<form")
	if !found {
		t.Fatalf("no answer form on the page: %s", body)
	}

	form, _, found = strings.Cut(form, "</form>")
	if !found {
		t.Fatalf("the answer form is unclosed: %s", form)
	}

	text := strings.Index(form, `name="answer_text"`)
	option := strings.Index(form, `name="answer"`)

	if text < 0 || option < 0 {
		t.Fatalf("the answer form is missing one of its controls: %s", form)
	}

	if text > option {
		t.Error("an option button precedes the free-text field, so pressing Enter in the field submits that option instead of what was typed")
	}
}
