package agent

// The ask: step: a question the pipeline asks, put to a person by ask_user's ladder minus the responder.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// StepQuestion is one ask: step, rendered.
type StepQuestion struct {
	Pipeline string
	Job      string
	Name     string
	Question string
	Options  []string
	Default  string
	// StepKey is where in the run the step sits; see store.Question.StepKey.
	StepKey string
	Wait    time.Duration
}

// AskForStep returns the answer to q: one this run already holds at this step, a --answer seed's, or a person's; the default when nobody answers in time; otherwise an error, unmarked because nobody said no.
func AskForStep(ctx context.Context, st store.Questions, q StepQuestion) (string, error) {
	runID := events.RunID(ctx)
	if runID == "" {
		return "", errors.New("this step is not running inside a recorded run, so there is nobody to ask")
	}

	grant := askGrant{defaultAnswer: q.Default, optionsRequired: len(q.Options) > 0, wait: q.Wait, answerFlags: AnswerFlags(ctx, q.Pipeline)}
	env := toolEnv{ask: askContext(st, q.Job, q.Name)}

	// Not the step's context, for ask_user's reason: a cancel between the INSERT and its read-back strands a row nobody holds.
	row, existing, err := st.AskQuestion(context.WithoutCancel(ctx), store.Question{
		RunID: runID, JobName: q.Job, AgentName: q.Name, Question: q.Question, Options: q.Options,
		OptionsRequired: grant.optionsRequired, Default: q.Default, StepKey: q.StepKey,
	})
	if err != nil {
		return "", err //nolint:wrapcheck // the store names the job
	}

	if row.Status != store.QuestionPending {
		return alreadySettled(ctx, row)
	}

	result := grant.settle(ctx, env, row, existing)

	err = env.ask.state.aborted()
	if err != nil {
		return "", err
	}

	if message, failed := result[resultErrorKey].(string); failed {
		grant.close(ctx, env, row, store.QuestionAborted, "", "step")

		return "", errors.New(strings.TrimPrefix(message, config.AskUserBuiltinName+": "))
	}

	answer, _ := result["answer"].(string)

	return answer, nil
}

// alreadySettled is a row this run settled at this step before: a resume's, or one answered between the insert and its read-back.
func alreadySettled(ctx context.Context, row store.Question) (string, error) {
	if row.Status == store.QuestionAnswered || row.Answer != "" {
		events.Note(ctx, events.NoteInfo, fmt.Sprintf("question %d: already answered in this run: %s", row.ID, row.Answer))

		return row.Answer, nil
	}

	return "", fmt.Errorf("question %d was %s with no answer: %s", row.ID, row.Status, row.Question)
}

// settle is ask's ladder for a pending row: only the asker tries the machines and announces, and anyone waits.
func (g askGrant) settle(ctx context.Context, env toolEnv, row store.Question, existing bool) map[string]any {
	if !existing {
		if result := g.answerFromMachines(ctx, env, row); result != nil {
			return result
		}

		defer g.announce(ctx, env, row)()
	}

	return g.waitForAnswer(ctx, env, row)
}
