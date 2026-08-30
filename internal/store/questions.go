package store

// questions: the record of every question an agent step asked its end user,
// and of what came back.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Question is one recorded ask_user call, and what became of it. Like an
// approval, the row IS the audit trail — but it records a FACT rather than a
// permission, so it carries the things an approval has no use for: the options
// the model offered, the default it declared, who or what answered, and the
// memo key that makes one question asked by twelve matrix cells one question.
type Question struct {
	ID      int64
	RunID   string
	JobName string
	// AgentName is the agent as the step KNOWS it (across: cells report under
	// their labelled identity), so a parked question names something a reader
	// can find in the plan.
	AgentName string
	Question  string
	Options   []string
	// OptionsRequired is recorded rather than only enforced in the asking
	// process, because the process that ANSWERS is usually a different one:
	// `steps answer` and the web UI both write this row directly, and a fence
	// only the asker knew about would be no fence on either of them.
	OptionsRequired bool
	Default         string
	Status          string // pending, answered, expired, aborted
	AskedAt         string
	AnsweredAt      string
	AnsweredBy      string
	Answer          string
}

// ErrQuestionNotPending is what every answer path gets when the row it aimed
// at has already resolved — a race between a person and a responder agent, or
// between a person and the deadline. A sentinel because the CLI and the web UI
// both report it as "somebody got there first" rather than as a failure.
var ErrQuestionNotPending = errors.New("question is not pending")

// AskQuestion records a pending question, or returns the one this run already
// asked.
//
// The memo is the row, not a map beside it. Within one run a question is
// identified by its memo key, and the unique index is what makes that true
// under concurrency: an across: matrix whose twelve cells each ask the same
// thing, or two in_parallel: branches racing, all reach the SAME row — the
// first insert wins and every other caller gets that row back to wait on.
// Twelve identical prompts to one person is the behavior that ends adoption of
// a feature, and a map in the asking process would not survive the second
// process anyway.
//
// existing reports whether the row was already there, which is what lets the
// caller tell "I am the one who asked this" from "I am waiting on somebody
// else's ask" — they are answered by the same row but reported to the model
// differently.
func (s *Store) AskQuestion(ctx context.Context, question Question) (Question, bool, error) {
	options, err := encodeOptions(question.Options)
	if err != nil {
		return Question{}, false, err
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO questions (run_id, job_name, agent_name, question, options, options_required,
		                       default_answer, memo_key, status, asked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)
		ON CONFLICT (run_id, memo_key) DO NOTHING
	`, question.RunID, question.JobName, question.AgentName, question.Question, options,
		question.OptionsRequired, nullableText(question.Default), question.MemoKey(), nowNano())
	if err != nil {
		return Question{}, false, fmt.Errorf("could not record question for job %q: %w", question.JobName, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return Question{}, false, fmt.Errorf("could not record question for job %q: %w", question.JobName, err)
	}

	stored, err := s.questionByMemo(ctx, question.RunID, question.MemoKey())
	if err != nil {
		return Question{}, false, err
	}

	return stored, affected == 0, nil
}

// AnswerQuestion records an answer against a pending question. It is the one
// write every ANSWERING channel shares — a seeded answer, a responder agent, a
// person at a TTY, `steps answer`, the web UI — so the options fence and the
// already-resolved race are decided once, here, rather than five times.
func (s *Store) AnswerQuestion(ctx context.Context, id int64, answer, by string) error {
	question, err := s.QuestionStatus(ctx, id)
	if err != nil {
		return err
	}

	if question.Status != "pending" {
		return fmt.Errorf("question %d: %w (already %s)", id, ErrQuestionNotPending, question.Status)
	}

	if question.OptionsRequired && !slices.Contains(question.Options, answer) {
		return fmt.Errorf("question %d: answer %q is not one of the offered options: %s",
			id, answer, strings.Join(question.Options, ", "))
	}

	return s.closeQuestion(ctx, id, "answered", answer, by)
}

// CloseQuestion resolves a question nobody answered: 'expired' when the wait
// ran out (carrying the declared default, if there was one, so the row says
// what the model was actually told) or 'aborted' when the step ended first.
//
// The aborted case is why this exists separately from AnswerQuestion. A
// question left `pending` after its step is gone is unanswerable, and showing
// it in `steps questions` as though somebody could still answer it is the same
// class of lie as presenting a default as a person's decision.
func (s *Store) CloseQuestion(ctx context.Context, id int64, status, answer, by string) error {
	return s.closeQuestion(ctx, id, status, answer, by)
}

func (s *Store) closeQuestion(ctx context.Context, id int64, status, answer, by string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE questions SET status = ?, answered_at = ?, answered_by = ?, answer = ?
		WHERE id = ? AND status = 'pending'
		  AND run_id IN (SELECT id FROM runs WHERE pipeline_id = ?)
	`, status, nowNano(), by, nullableText(answer), id, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not resolve question %d: %w", id, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("could not resolve question %d: %w", id, err)
	}

	if affected == 0 {
		return fmt.Errorf("question %d: %w (already resolved, or never existed)", id, ErrQuestionNotPending)
	}

	return nil
}

// QuestionStatus reads one question's current state.
func (s *Store) QuestionStatus(ctx context.Context, id int64) (Question, error) {
	question, err := s.scanQuestion(s.db.QueryRowContext(ctx, questionColumns+`
		WHERE q.id = ? AND r.pipeline_id = ?
	`, id, s.pipelineID))
	if err != nil {
		return Question{}, fmt.Errorf("could not read question %d: %w", id, err)
	}

	return question, nil
}

// questionByMemo reads back the row a memo key resolved to, which is the one
// AskQuestion just inserted or the one that beat it there.
func (s *Store) questionByMemo(ctx context.Context, runID, memoKey string) (Question, error) {
	question, err := s.scanQuestion(s.db.QueryRowContext(ctx, questionColumns+`
		WHERE q.run_id = ? AND q.memo_key = ? AND r.pipeline_id = ?
	`, runID, memoKey, s.pipelineID))
	if err != nil {
		return Question{}, fmt.Errorf("could not read the question just recorded: %w", err)
	}

	return question, nil
}

// PendingQuestions lists every question still waiting, oldest first.
func (s *Store) PendingQuestions(ctx context.Context) ([]Question, error) {
	return collect(ctx, s.db, "pending questions", questionColumns+`
		WHERE q.status = 'pending' AND r.pipeline_id = ? ORDER BY q.id
	`, []any{s.pipelineID}, func(rows *sql.Rows) (Question, error) {
		return s.scanQuestion(rows)
	})
}

// AllQuestions lists every question and what became of it, newest first — the
// audit trail PendingQuestions deliberately does not carry.
func (s *Store) AllQuestions(ctx context.Context, limit int) ([]Question, error) {
	return collect(ctx, s.db, "questions", questionColumns+`
		WHERE r.pipeline_id = ? ORDER BY q.id DESC LIMIT ?
	`, []any{s.pipelineID, limit}, func(rows *sql.Rows) (Question, error) {
		return s.scanQuestion(rows)
	})
}

// questionColumns is the SELECT every read shares. The join is how a question
// is pipeline-scoped: it is run-scoped, like run_steps and agent_usage, and
// reaches its pipeline through the run it belongs to rather than carrying a
// second copy of the answer.
const questionColumns = `
	SELECT q.id, q.run_id, q.job_name, q.agent_name, q.question, q.options, q.options_required,
	       COALESCE(q.default_answer, ''), q.status, q.asked_at,
	       COALESCE(q.answered_at, ''), COALESCE(q.answered_by, ''), COALESCE(q.answer, '')
	FROM questions q JOIN runs r ON r.id = q.run_id
`

// scanner is the one method sql.Row and sql.Rows share, so a row read the
// same way from either.
type scanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanQuestion(row scanner) (Question, error) {
	var (
		question Question
		options  string
	)

	err := row.Scan(&question.ID, &question.RunID, &question.JobName, &question.AgentName,
		&question.Question, &options, &question.OptionsRequired, &question.Default,
		&question.Status, &question.AskedAt, &question.AnsweredAt, &question.AnsweredBy, &question.Answer)
	if err != nil {
		return Question{}, err //nolint:wrapcheck // every caller wraps with what it was reading
	}

	question.Options, err = decodeOptions(options)

	return question, err
}

// MemoKey identifies a question WITHIN one run: the same text offering the
// same options is the same question, however many steps ask it.
//
// The options are part of the key, not decoration. "Which environment?" over
// {staging, prod} and the same sentence over {staging, prod, canary} are two
// different asks, and answering the second with the first's answer would be
// the runtime deciding a question nobody put that way.
//
// Hashed rather than stored raw because it is a unique-index key and a
// question may be a paragraph; the text itself is in the row beside it.
func (q Question) MemoKey() string {
	sum := sha256.Sum256([]byte(q.Question + "\x00" + strings.Join(q.Options, "\x00")))

	return hex.EncodeToString(sum[:])
}

// encodeOptions stores the offered list as JSON. Always a JSON array, never
// NULL: "the model offered nothing" and "we did not record what it offered"
// are different facts, and an empty array says the first one.
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

// nullableText keeps "not declared" out of the answer columns as NULL rather
// than as an empty string, so a question answered with deliberate silence
// stays tellable apart from one nobody reached.
func nullableText(value string) any {
	if value == "" {
		return nil
	}

	return value
}
