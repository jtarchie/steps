package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestAnAnswerThatTookNoEffectIsNotRecorded: a second answer to one question is settled on with a redirect, but it changed nothing, so the log keeps only the answer that landed.
func TestAnAnswerThatTookNoEffectIsNotRecorded(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	question := askOne(t, pipeline)

	for _, target := range []string{"/p/demo/questions/1", "/p/demo/questions/1", "/p/demo/questions/999"} {
		if code := post(t, server, target, map[string]string{"answer": "minor"}); code != http.StatusSeeOther {
			t.Fatalf("POST %s = %d, want a redirect", target, code)
		}
	}

	entries, err := pipeline.Store.Reader().Activity(context.Background(), store.ActivityFilter{Action: "answer"})
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}

	if len(entries) != 1 || entries[0].Target != "question 1" || question.ID != 1 {
		t.Errorf("activity = %+v, want the one answer that landed", entries)
	}
}

type failingAnswers struct{ store.Store }

func (failingAnswers) AnswerQuestion(context.Context, int64, string, string) error {
	return errors.New(leakyDetail)
}

// TestAnswerQuestionReportsAStoreFailureAsTheDaemons: an answer the store failed to write is not the answerer's mistake; it was a 400 carrying the driver's text, with nothing in the daemon's log. Serial: the logger is the process's.
func TestAnswerQuestionReportsAStoreFailureAsTheDaemons(t *testing.T) {
	logs := captureLogs(t)

	_, pipeline := testPipeline(t)
	askOne(t, pipeline)
	pipeline.Store = failingAnswers{pipeline.Store}

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if code := post(t, server, "/p/demo/questions/1", map[string]string{"answer": "minor"}); code != http.StatusInternalServerError {
		t.Errorf("an answer the store failed to record = %d, want 500", code)
	}

	if !strings.Contains(logs.String(), "web.request_failed") || !strings.Contains(logs.String(), leakyDetail) {
		t.Errorf("the failure was not logged with its cause:\n%s", logs.String())
	}
}
