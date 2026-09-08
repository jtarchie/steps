package storetest

// Questions: the memo a run asks through, and the listing that answers it.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// startQuestionRun opens a st with one running run to hang questions off,
// since a question is run-scoped and the foreign key means it.
func (s suite) startQuestionRun(t *testing.T) store.Store {
	t.Helper()

	st := s.open(t, "test")
	t.Cleanup(func() { _ = st.Close() })

	err := st.StartRun(context.Background(), "run-1", "release-note", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	return st
}

func askBump(t *testing.T, st store.Store, runID string) (store.Question, bool) {
	t.Helper()

	question, existing, err := st.AskQuestion(context.Background(), store.Question{
		RunID: runID, JobName: "release-note", AgentName: "writer",
		Question: "Is this release a major or a minor bump?",
		Options:  []string{"major", "minor"},
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	return question, existing
}

// TestAskQuestionMemoizesWithinARun is the whole reason the memo is a unique
// index rather than a map: the second ask of the same question must land on
// the FIRST one's row, so a matrix of cells asks a person once.
func (s suite) TestAskQuestionMemoizesWithinARun(t *testing.T) {
	t.Parallel()

	st := s.startQuestionRun(t)

	first, existing := askBump(t, st, "run-1")
	if existing {
		t.Error("the first ask reported an existing question")
	}

	second, existing := askBump(t, st, "run-1")
	if !existing {
		t.Error("the second ask of the same question did not report the existing row")
	}

	if second.ID != first.ID {
		t.Errorf("the second ask created question %d, want the existing %d", second.ID, first.ID)
	}
}

// TestAskQuestionMemoIsPerRun holds the other half: an answer given yesterday
// must not stand in silently for a question asked today.
func (s suite) TestAskQuestionMemoIsPerRun(t *testing.T) {
	t.Parallel()

	st := s.startQuestionRun(t)

	err := st.StartRun(context.Background(), "run-2", "release-note", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	first, _ := askBump(t, st, "run-1")
	second, existing := askBump(t, st, "run-2")

	if existing || second.ID == first.ID {
		t.Errorf("a new run reused question %d; a new run is a new set of circumstances", first.ID)
	}
}

// TestAskQuestionMemoSeparatesDifferentOptions pins that the offered list is
// part of a question's identity. The same sentence over a different set of
// options is a different ask, and answering it from the first one's row would
// be the runtime deciding a question nobody put that way.
func (s suite) TestAskQuestionMemoSeparatesDifferentOptions(t *testing.T) {
	t.Parallel()

	st := s.startQuestionRun(t)

	first, _ := askBump(t, st, "run-1")

	widened, existing, err := st.AskQuestion(context.Background(), store.Question{
		RunID: "run-1", JobName: "release-note", AgentName: "writer",
		Question: "Is this release a major or a minor bump?",
		Options:  []string{"major", "minor", "patch"},
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	if existing || widened.ID == first.ID {
		t.Error("a question offering a different option set reused the earlier row")
	}
}

// TestAskQuestionMemoHoldsUnderConcurrentAskers is the case the unique index
// exists for and a map could not cover: in_parallel: branches racing to ask
// the same thing must not stampede a person.
func (s suite) TestAskQuestionMemoHoldsUnderConcurrentAskers(t *testing.T) {
	t.Parallel()

	st := s.startQuestionRun(t)

	const askers = 8

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids = map[int64]int{}
	)

	for range askers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			question, _ := askBump(t, st, "run-1")

			mu.Lock()
			defer mu.Unlock()

			ids[question.ID]++
		}()
	}

	wg.Wait()

	if len(ids) != 1 {
		t.Errorf("%d concurrent askers produced %d questions, want 1", askers, len(ids))
	}
}

// TestAnswerQuestionRecordsWhoAndWhat covers the ordinary answer, and that a
// second answer is refused rather than overwriting the first.
func (s suite) TestAnswerQuestionRecordsWhoAndWhat(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.startQuestionRun(t)
	question, _ := askBump(t, st, "run-1")

	err := st.AnswerQuestion(ctx, question.ID, "minor", "jtarchie")
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}

	answered, err := st.QuestionStatus(ctx, question.ID)
	if err != nil {
		t.Fatalf("QuestionStatus: %v", err)
	}

	if answered.Status != "answered" || answered.Answer != "minor" || answered.AnsweredBy != "jtarchie" {
		t.Errorf("question = status %q answer %q by %q, want answered/minor/jtarchie",
			answered.Status, answered.Answer, answered.AnsweredBy)
	}

	if answered.AnsweredAt == "" {
		t.Error("an answered question recorded no answered_at")
	}

	err = st.AnswerQuestion(ctx, question.ID, "major", "someone-else")
	if !errors.Is(err, store.ErrQuestionNotPending) {
		t.Errorf("answering an already-answered question = %v, want store.ErrQuestionNotPending", err)
	}

	if again, _ := st.QuestionStatus(ctx, question.ID); again.Answer != "minor" {
		t.Errorf("the second answer overwrote the first: %q", again.Answer)
	}
}

// TestAnswerQuestionEnforcesOptionsRequired proves the fence lives in the row
// rather than in the asking process — every channel that answers writes
// through here, including ones in another process entirely.
func (s suite) TestAnswerQuestionEnforcesOptionsRequired(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.startQuestionRun(t)

	question, _, err := st.AskQuestion(ctx, store.Question{
		RunID: "run-1", JobName: "release-note", AgentName: "writer",
		Question: "Which environment?", Options: []string{"staging", "prod"},
		OptionsRequired: true,
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	err = st.AnswerQuestion(ctx, question.ID, "canary", "jtarchie")
	if err == nil {
		t.Fatal("an off-list answer was accepted for an options_required question")
	}

	if still, _ := st.QuestionStatus(ctx, question.ID); still.Status != "pending" {
		t.Errorf("a refused answer resolved the question anyway: %q", still.Status)
	}

	err = st.AnswerQuestion(ctx, question.ID, "prod", "jtarchie")
	if err != nil {
		t.Fatalf("an on-list answer was refused: %v", err)
	}
}

// TestCloseQuestionRecordsWhatTheModelWasTold covers the two ways a question
// resolves without an answerer: the wait ran out (and the model was handed the
// declared default), and the step ended first.
func (s suite) TestCloseQuestionRecordsWhatTheModelWasTold(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.startQuestionRun(t)

	expired, _ := askBump(t, st, "run-1")

	err := st.CloseQuestion(ctx, expired.ID, "expired", "minor", "default")
	if err != nil {
		t.Fatalf("CloseQuestion: %v", err)
	}

	got, err := st.QuestionStatus(ctx, expired.ID)
	if err != nil {
		t.Fatalf("QuestionStatus: %v", err)
	}

	if got.Status != "expired" || got.Answer != "minor" || got.AnsweredBy != "default" {
		t.Errorf("expired question = status %q answer %q by %q, want expired/minor/default",
			got.Status, got.Answer, got.AnsweredBy)
	}

	// A question the step abandoned must not keep showing up as answerable.
	aborted, _, err := st.AskQuestion(ctx, store.Question{
		RunID: "run-1", JobName: "release-note", AgentName: "writer", Question: "Anything else?",
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	err = st.CloseQuestion(ctx, aborted.ID, "aborted", "", "step")
	if err != nil {
		t.Fatalf("CloseQuestion: %v", err)
	}

	pending, err := st.Questions(ctx, true, 0)
	if err != nil {
		t.Fatalf("Questions: %v", err)
	}

	if len(pending) != 0 {
		t.Errorf("Questions(pendingOnly) listed %d resolved questions: %+v", len(pending), pending)
	}
}

// TestPendingQuestionsAreScopedToTheirPipeline holds the scoping rule against
// a shared state file: a question belongs to one pipeline, reached through its
// run, and the other pipeline in the same database must not see it.
func (s suite) TestPendingQuestionsAreScopedToTheirPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	mine := s.open(t, "test")
	defer func() { _ = mine.Close() }()

	theirs := s.open(t, "other")

	defer func() { _ = theirs.Close() }()

	err := mine.StartRun(ctx, "run-1", "release-note", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	question, _ := askBump(t, mine, "run-1")

	pending, err := theirs.Questions(ctx, true, 0)
	if err != nil {
		t.Fatalf("Questions: %v", err)
	}

	if len(pending) != 0 {
		t.Errorf("the other pipeline saw %d of this one's questions", len(pending))
	}

	err = theirs.AnswerQuestion(ctx, question.ID, "minor", "stranger")
	if err == nil {
		t.Error("the other pipeline answered this one's question")
	}

	if mineStill, _ := mine.QuestionStatus(ctx, question.ID); mineStill.Status != "pending" {
		t.Errorf("this pipeline's question was resolved from outside: %q", mineStill.Status)
	}
}

// TestAllQuestionsListsWhatIsWaitingFirst: the listing is capped and it is the
// only route the UI offers for answering, so recency ordering alone would push
// a parked question off the page while the nav badge still counted it.
func (s suite) TestAllQuestionsListsWhatIsWaitingFirst(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.startQuestionRun(t)

	parked, _ := askBump(t, st, "run-1")

	for i := range 5 {
		later, _, err := st.AskQuestion(ctx, store.Question{
			RunID: "run-1", JobName: "release-note", AgentName: "writer",
			Question: fmt.Sprintf("Later question %d?", i),
		})
		if err != nil {
			t.Fatalf("AskQuestion: %v", err)
		}

		err = st.AnswerQuestion(ctx, later.ID, "sure", "jtarchie")
		if err != nil {
			t.Fatalf("AnswerQuestion: %v", err)
		}
	}

	listed, err := st.Questions(ctx, false, 3)
	if err != nil {
		t.Fatalf("Questions: %v", err)
	}

	if len(listed) == 0 || listed[0].ID != parked.ID {
		t.Errorf("Questions listed %+v first, want the pending question %d", listed, parked.ID)
	}
}

// TestPendingQuestionsAreNotCapped: the waiting list is what the nav badge
// counts and the only page that can unpark a run, so it takes every pending
// question however many there are. Zero means no limit here the way it does
// everywhere else — a listing that quietly capped itself would hide a parked
// run behind a count that still included it.
func (s suite) TestPendingQuestionsAreNotCapped(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.startQuestionRun(t)

	const asked = 4

	for i := range asked {
		_, _, err := st.AskQuestion(ctx, store.Question{
			RunID: "run-1", JobName: "release-note", AgentName: "writer",
			Question: fmt.Sprintf("store.Question %d?", i),
		})
		if err != nil {
			t.Fatalf("AskQuestion: %v", err)
		}
	}

	pending, err := st.Questions(ctx, true, 0)
	if err != nil {
		t.Fatalf("Questions: %v", err)
	}

	if len(pending) != asked {
		t.Fatalf("got %d pending questions, want all %d", len(pending), asked)
	}

	// Oldest first: the order somebody should answer them in.
	for i := 1; i < len(pending); i++ {
		if pending[i-1].ID > pending[i].ID {
			t.Errorf("pending questions are not oldest-first: %d before %d", pending[i-1].ID, pending[i].ID)
		}
	}
}
