package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// The asker announces once, until when the wait ends, and the row remembers it; the second asker of the same row, a seeded answer and a person at this terminal announce nothing.
func TestAskUserAnnouncesOnlyWhatAPersonMustAnswer(t *testing.T) {
	t.Parallel()

	fixture := newAskFixture(t)

	var (
		calls    atomic.Int32
		deadline atomic.Value
	)

	fixture.ctx = WithParkAnnouncer(fixture.ctx, func(_ context.Context, row store.Question, expiresAt time.Time) bool {
		calls.Add(1)
		deadline.Store(expiresAt.Sub(row.AskedAt))

		return true
	})

	fixture.ask(impatient("patch"), "Which bump?")

	if calls.Load() != 1 || deadline.Load() != time.Millisecond {
		t.Errorf("announced %d times expiring %v after asked, want once, at the grant's wait", calls.Load(), deadline.Load())
	}

	row, _ := fixture.store.QuestionStatus(fixture.ctx, 1)
	if row.NotifiedAt.IsZero() {
		t.Error("an announced question reads never notified")
	}

	fixture.ask(impatient("patch"), "Which bump?")

	seeded := WithAnswerSeeds(fixture.ctx, []AnswerSeed{{Match: "seeded", Answer: "yes"}})
	impatient("").ask(seeded, map[string]any{askUserQuestionArg: "Is this seeded?"}, fixture.env)

	fixture.env.ask.prompt = func(context.Context, store.Question) (string, bool) { return "typed", true }
	fixture.ask(askGrant{wait: 5 * time.Second}, "Asked at the terminal?")

	if calls.Load() != 1 {
		t.Errorf("announced %d times, want only the first question: a memo, a seed and a terminal need nobody told", calls.Load())
	}
}

// A wait that finds its question already asked counts from when it was asked, as an approval's does: a resumed wait is the same wait.
func TestAskUserWaitCountsFromWhenItWasAsked(t *testing.T) {
	t.Parallel()

	asked := time.Now().Add(-time.Hour)
	grant := askGrant{wait: time.Minute}

	if got := grant.deadline(store.Question{AskedAt: asked}); !got.Equal(asked.Add(time.Minute)) {
		t.Errorf("deadline = %v, want an hour-old question's to have passed at %v", got, asked.Add(time.Minute))
	}

	if got := time.Until(grant.deadline(store.Question{})); got < 50*time.Second {
		t.Errorf("a question with no recorded time waits %v, want the whole minute", got)
	}
}
