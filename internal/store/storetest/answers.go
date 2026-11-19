package storetest

import (
	"errors"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestAnswerQuestionNamesWhyItRefused: the web UI shows an off-list answer as the answerer's mistake and settles on a question that is gone, so each refusal must carry its own sentinel; anything else it reports as the daemon failing.
func (s suite) TestAnswerQuestionNamesWhyItRefused(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
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
	if !errors.Is(err, store.ErrAnswerNotOffered) || errors.Is(err, store.ErrQuestionNotPending) {
		t.Errorf("an off-list answer = %v, want store.ErrAnswerNotOffered alone", err)
	}

	err = st.AnswerQuestion(ctx, question.ID+1000, "prod", "jtarchie")
	if !errors.Is(err, store.ErrQuestionNotPending) {
		t.Errorf("answering a question nobody asked = %v, want store.ErrQuestionNotPending", err)
	}

	err = st.AnswerQuestion(ctx, question.ID, "prod", "jtarchie")
	if err != nil {
		t.Fatalf("an on-list answer was refused: %v", err)
	}

	err = st.AnswerQuestion(ctx, question.ID, "canary", "someone-else")
	if !errors.Is(err, store.ErrQuestionNotPending) || errors.Is(err, store.ErrAnswerNotOffered) {
		t.Errorf("an off-list answer to an answered question = %v, want store.ErrQuestionNotPending alone", err)
	}
}
