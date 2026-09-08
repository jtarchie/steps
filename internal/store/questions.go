package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	// `steps questions answer` and the web UI both write this row directly, and a fence
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
