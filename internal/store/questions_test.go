package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// startQuestionRun opens a store with one running run to hang questions off,
// since a question is run-scoped and the foreign key means it.
func startQuestionRun(t *testing.T) *Store {
	t.Helper()

	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	t.Cleanup(func() { _ = store.Close() })

	err := store.StartRun(context.Background(), "run-1", "release-note", "/tmp/ws")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	return store
}

func askBump(t *testing.T, store *Store, runID string) (Question, bool) {
	t.Helper()

	question, existing, err := store.AskQuestion(context.Background(), Question{
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
func TestAskQuestionMemoizesWithinARun(t *testing.T) {
	t.Parallel()

	store := startQuestionRun(t)

	first, existing := askBump(t, store, "run-1")
	if existing {
		t.Error("the first ask reported an existing question")
	}

	second, existing := askBump(t, store, "run-1")
	if !existing {
		t.Error("the second ask of the same question did not report the existing row")
	}

	if second.ID != first.ID {
		t.Errorf("the second ask created question %d, want the existing %d", second.ID, first.ID)
	}
}

// TestAskQuestionMemoIsPerRun holds the other half: an answer given yesterday
// must not stand in silently for a question asked today.
func TestAskQuestionMemoIsPerRun(t *testing.T) {
	t.Parallel()

	store := startQuestionRun(t)

	err := store.StartRun(context.Background(), "run-2", "release-note", "/tmp/ws")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	first, _ := askBump(t, store, "run-1")
	second, existing := askBump(t, store, "run-2")

	if existing || second.ID == first.ID {
		t.Errorf("a new run reused question %d; a new run is a new set of circumstances", first.ID)
	}
}

// TestAskQuestionMemoSeparatesDifferentOptions pins that the offered list is
// part of a question's identity. The same sentence over a different set of
// options is a different ask, and answering it from the first one's row would
// be the runtime deciding a question nobody put that way.
func TestAskQuestionMemoSeparatesDifferentOptions(t *testing.T) {
	t.Parallel()

	store := startQuestionRun(t)

	first, _ := askBump(t, store, "run-1")

	widened, existing, err := store.AskQuestion(context.Background(), Question{
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
func TestAskQuestionMemoHoldsUnderConcurrentAskers(t *testing.T) {
	t.Parallel()

	store := startQuestionRun(t)

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

			question, _ := askBump(t, store, "run-1")

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
func TestAnswerQuestionRecordsWhoAndWhat(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := startQuestionRun(t)
	question, _ := askBump(t, store, "run-1")

	err := store.AnswerQuestion(ctx, question.ID, "minor", "jtarchie")
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}

	answered, err := store.QuestionStatus(ctx, question.ID)
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

	err = store.AnswerQuestion(ctx, question.ID, "major", "someone-else")
	if !errors.Is(err, ErrQuestionNotPending) {
		t.Errorf("answering an already-answered question = %v, want ErrQuestionNotPending", err)
	}

	if again, _ := store.QuestionStatus(ctx, question.ID); again.Answer != "minor" {
		t.Errorf("the second answer overwrote the first: %q", again.Answer)
	}
}

// TestAnswerQuestionEnforcesOptionsRequired proves the fence lives in the row
// rather than in the asking process — every channel that answers writes
// through here, including ones in another process entirely.
func TestAnswerQuestionEnforcesOptionsRequired(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := startQuestionRun(t)

	question, _, err := store.AskQuestion(ctx, Question{
		RunID: "run-1", JobName: "release-note", AgentName: "writer",
		Question: "Which environment?", Options: []string{"staging", "prod"},
		OptionsRequired: true,
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	err = store.AnswerQuestion(ctx, question.ID, "canary", "jtarchie")
	if err == nil {
		t.Fatal("an off-list answer was accepted for an options_required question")
	}

	if still, _ := store.QuestionStatus(ctx, question.ID); still.Status != "pending" {
		t.Errorf("a refused answer resolved the question anyway: %q", still.Status)
	}

	err = store.AnswerQuestion(ctx, question.ID, "prod", "jtarchie")
	if err != nil {
		t.Fatalf("an on-list answer was refused: %v", err)
	}
}

// TestCloseQuestionRecordsWhatTheModelWasTold covers the two ways a question
// resolves without an answerer: the wait ran out (and the model was handed the
// declared default), and the step ended first.
func TestCloseQuestionRecordsWhatTheModelWasTold(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := startQuestionRun(t)

	expired, _ := askBump(t, store, "run-1")

	err := store.CloseQuestion(ctx, expired.ID, "expired", "minor", "default")
	if err != nil {
		t.Fatalf("CloseQuestion: %v", err)
	}

	got, err := store.QuestionStatus(ctx, expired.ID)
	if err != nil {
		t.Fatalf("QuestionStatus: %v", err)
	}

	if got.Status != "expired" || got.Answer != "minor" || got.AnsweredBy != "default" {
		t.Errorf("expired question = status %q answer %q by %q, want expired/minor/default",
			got.Status, got.Answer, got.AnsweredBy)
	}

	// A question the step abandoned must not keep showing up as answerable.
	aborted, _, err := store.AskQuestion(ctx, Question{
		RunID: "run-1", JobName: "release-note", AgentName: "writer", Question: "Anything else?",
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	err = store.CloseQuestion(ctx, aborted.ID, "aborted", "", "step")
	if err != nil {
		t.Fatalf("CloseQuestion: %v", err)
	}

	pending, err := store.PendingQuestions(ctx)
	if err != nil {
		t.Fatalf("PendingQuestions: %v", err)
	}

	if len(pending) != 0 {
		t.Errorf("PendingQuestions listed %d resolved questions: %+v", len(pending), pending)
	}
}

// TestPendingQuestionsAreScopedToTheirPipeline holds the scoping rule against
// a shared state file: a question belongs to one pipeline, reached through its
// run, and the other pipeline in the same database must not see it.
func TestPendingQuestionsAreScopedToTheirPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")

	mine := mustOpenStore(t, path)
	defer func() { _ = mine.Close() }()

	theirs, err := OpenStore(path, "other")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	defer func() { _ = theirs.Close() }()

	err = mine.StartRun(ctx, "run-1", "release-note", "/tmp/ws")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	question, _ := askBump(t, mine, "run-1")

	pending, err := theirs.PendingQuestions(ctx)
	if err != nil {
		t.Fatalf("PendingQuestions: %v", err)
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

// TestQuestionsAreReapedWithTheirRun proves the retention story the table was
// shaped for: it is run-scoped, so a pruned run takes its questions with it
// and nothing has to remember a second DELETE.
func TestQuestionsAreReapedWithTheirRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	for build := 1; build <= 6; build++ {
		syntheticBuild(ctx, t, store, "answer-mention", build)
	}

	err := store.PruneRuns(ctx, "answer-mention", 2, "")
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}

	var count int

	err = store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM questions`).Scan(&count)
	if err != nil {
		t.Fatalf("count questions: %v", err)
	}

	if count != 2 {
		t.Errorf("%d questions survived a prune to 2 runs, want 2", count)
	}
}
