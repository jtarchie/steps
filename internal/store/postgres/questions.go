package postgres

// questions: every question an agent step asked its end user, and what came
// back.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// AskQuestion records a pending question, or returns the one this run
// already asked under the same memo key. The INSERT..SELECT is what scopes it
// to this pipeline; the unique index is what makes concurrent askers share
// one row.
func (s *Store) AskQuestion(ctx context.Context, question store.Question) (store.Question, bool, error) {
	options, err := encodeOptions(question.Options)
	if err != nil {
		return store.Question{}, false, err
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO questions (run_id, job_name, agent_name, question, options, options_required,
		                       default_answer, memo_key, status, asked_at)
		SELECT $1::text, $2::text, $3::text, $4::text, $5::text, $6::boolean, $7::text, $8::text, 'pending', $9::timestamptz
		WHERE EXISTS (SELECT 1 FROM runs WHERE id = $1 AND pipeline_id = $10)
		ON CONFLICT (run_id, memo_key) DO NOTHING
	`, question.RunID, question.JobName, clean(question.AgentName), clean(question.Question), options,
		question.OptionsRequired, nullable(question.Default), question.MemoKey(), now(),
		s.pipelineID)
	if err != nil {
		return store.Question{}, false, fmt.Errorf("could not record question for job %q: %w", question.JobName, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return store.Question{}, false, fmt.Errorf("could not record question for job %q: %w", question.JobName, err)
	}

	stored, err := s.questionByMemo(ctx, question.RunID, question.MemoKey())
	if err != nil {
		return store.Question{}, false, err
	}

	return stored, affected == 0, nil
}

// AnswerQuestion records an answer against a pending question, holding it to
// the offered options when they are required.
func (s *Store) AnswerQuestion(ctx context.Context, id int64, answer, by string) error {
	question, err := s.QuestionStatus(ctx, id)
	if err != nil {
		return err
	}

	if question.Status != "pending" {
		return fmt.Errorf("question %d: %w (already %s)", id, store.ErrQuestionNotPending, question.Status)
	}

	if question.OptionsRequired && !slices.Contains(question.Options, answer) {
		return fmt.Errorf("question %d: answer %q is not one of the offered options: %s",
			id, answer, strings.Join(question.Options, ", "))
	}

	return s.closeQuestion(ctx, id, "answered", answer, by)
}

// CloseQuestion resolves a question nobody answered; see the sqlite driver's
// for why this stays apart from AnswerQuestion.
func (s *Store) CloseQuestion(ctx context.Context, id int64, status, answer, by string) error {
	return s.closeQuestion(ctx, id, status, answer, by)
}

func (s *Store) closeQuestion(ctx context.Context, id int64, status, answer, by string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE questions SET status = $1, answered_at = $2, answered_by = $3, answer = $4
		WHERE id = $5 AND status = 'pending'
		  AND EXISTS (SELECT 1 FROM runs WHERE id = questions.run_id AND pipeline_id = $6)
	`, status, now(), clean(by), nullable(answer), id, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not resolve question %d: %w", id, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("could not resolve question %d: %w", id, err)
	}

	if affected == 0 {
		return fmt.Errorf("question %d: %w (already resolved, or never existed)", id, store.ErrQuestionNotPending)
	}

	return nil
}

// QuestionStatus reads one question's current state.
func (s *Store) QuestionStatus(ctx context.Context, id int64) (store.Question, error) {
	question, err := scanQuestion(s.db.QueryRowContext(ctx, questionColumns+`
		WHERE q.id = $1 AND r.pipeline_id = $2
	`, id, s.pipelineID))
	if err != nil {
		return store.Question{}, fmt.Errorf("could not read question %d: %w", id, err)
	}

	return question, nil
}

func (s *Store) questionByMemo(ctx context.Context, runID, memoKey string) (store.Question, error) {
	question, err := scanQuestion(s.db.QueryRowContext(ctx, questionColumns+`
		WHERE q.run_id = $1 AND q.memo_key = $2 AND r.pipeline_id = $3
	`, runID, memoKey, s.pipelineID))
	if err != nil {
		return store.Question{}, fmt.Errorf("could not read the question just recorded: %w", err)
	}

	return question, nil
}

// Questions lists what a pipeline has asked: waiting ones oldest first, or
// everything with the waiting ones first and the rest newest first.
func (s *Store) Questions(ctx context.Context, pendingOnly bool, limit int) ([]store.Question, error) {
	where, order, what := `q.status = 'pending'`, `q.id`, "pending questions"
	if !pendingOnly {
		where, order, what = `TRUE`, `(q.status = 'pending') DESC, q.id DESC`, "questions"
	}

	return collect(ctx, s.db, what, questionColumns+`
		WHERE `+where+` AND r.pipeline_id = $1
		ORDER BY `+order+` LIMIT $2
	`, []any{s.pipelineID, rowLimit(limit)}, func(rows *sql.Rows) (store.Question, error) {
		return scanQuestion(rows)
	})
}

// questionColumns is the SELECT every read shares; the join is how a
// question is pipeline-scoped.
const questionColumns = `
	SELECT q.id, q.run_id, q.job_name, q.agent_name, q.question, q.options, q.options_required,
	       COALESCE(q.default_answer, ''), q.status, q.asked_at,
	       q.answered_at, COALESCE(q.answered_by, ''), COALESCE(q.answer, '')
	FROM questions q JOIN runs r ON r.id = q.run_id
`

// scanQuestion renders the timestamps in the sqlite driver's sortableNano,
// the layout it stores this table's in.
func scanQuestion(row rowScanner) (store.Question, error) {
	var (
		question store.Question
		options  string
		asked    time.Time
		answered sql.NullTime
	)

	err := row.Scan(&question.ID, &question.RunID, &question.JobName, &question.AgentName,
		&question.Question, &options, &question.OptionsRequired, &question.Default,
		&question.Status, &asked, &answered, &question.AnsweredBy, &question.Answer)
	if err != nil {
		return store.Question{}, err //nolint:wrapcheck // every caller wraps with what it was reading
	}

	question.AskedAt = asked.UTC().Format(sortableNano)
	question.AnsweredAt = stamp(answered, sortableNano)
	question.Options, err = decodeOptions(options)

	return question, err
}

func encodeOptions(options []string) (string, error) {
	if len(options) == 0 {
		return "[]", nil
	}

	encoded, err := json.Marshal(options)
	if err != nil {
		return "", fmt.Errorf("could not record the question's options: %w", err)
	}

	return string(encoded), nil
}

func decodeOptions(encoded string) ([]string, error) {
	if encoded == "" || encoded == "[]" {
		return nil, nil
	}

	var options []string

	err := json.Unmarshal([]byte(encoded), &options)
	if err != nil {
		return nil, fmt.Errorf("could not read the question's options: %w", err)
	}

	return options, nil
}
