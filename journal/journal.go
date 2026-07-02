// Package journal is the run's source of truth: an append-only event log.
// The run context is a fold over handler_finished events — resume is
// fold + continue, never replay of side effects.
package journal

import (
	"context"
	"encoding/json"
	"time"
)

// EventType enumerates journal event types.
type EventType string

const (
	RunStarted      EventType = "run_started"
	StateEntered    EventType = "state_entered"
	HandlerFinished EventType = "handler_finished"
	HandlerFailed   EventType = "handler_failed"
	RetryScheduled  EventType = "retry_scheduled"
	TransitionFired EventType = "transition_fired"
	RunParked       EventType = "run_parked"
	RunResumed      EventType = "run_resumed"
	RunFinished     EventType = "run_finished"
)

// Event is one journal entry. Data is event-type-specific JSON.
type Event struct {
	RunID string         `json:"run_id"`
	Seq   int            `json:"seq"`
	Type  EventType      `json:"type"`
	Time  time.Time      `json:"time"`
	Data  map[string]any `json:"data"`
}

// Run is the queryable run row (denormalized from the journal for listing).
type Run struct {
	ID           string
	Machine      string
	Hash         string
	YAML         []byte // machine definition pinned at start; resume needs no file
	Status       string // running | parked | done | failed
	CurrentState string
	Created      time.Time
	Updated      time.Time
}

// Run statuses.
const (
	StatusRunning = "running"
	StatusParked  = "parked"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// Store persists runs and their journals.
type Store interface {
	CreateRun(ctx context.Context, run *Run) error
	UpdateRun(ctx context.Context, id, status, currentState string) error
	GetRun(ctx context.Context, id string) (*Run, error)
	ListRuns(ctx context.Context) ([]*Run, error)
	// Append assigns and returns the next sequence number.
	Append(ctx context.Context, ev *Event) (int, error)
	Events(ctx context.Context, runID string) ([]*Event, error)
	Close() error
}

// Message is the normalized conversation message stored per state execution.
// history renders it; adopt replays it; providers convert it back.
type Message struct {
	Role        string       `json:"role"` // user | model
	Text        string       `json:"text,omitempty"`
	ToolCalls   []ToolCall   `json:"tool_calls,omitempty"`
	ToolResults []ToolResult `json:"tool_results,omitempty"`
	// Thought marks reasoning-channel content: journaled for audit, but
	// never replayed on adopt and excluded from history by default —
	// scratch thinking is not context, and replaying it re-bills it.
	Thought bool `json:"thought,omitempty"`
}

// ToolCall is a model-authored tool invocation.
type ToolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// ToolResult is the result returned for a tool call.
type ToolResult struct {
	Name   string         `json:"name"`
	Result map[string]any `json:"result,omitempty"`
}

// Usage is cumulative token/cost accounting.
type Usage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Cost         float64 `json:"cost"`
}

// Total returns all tokens.
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

// Add accumulates.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.Cost += o.Cost
}

// DecodeData unmarshals an event's Data into a typed struct via JSON.
func DecodeData(ev *Event, into any) error {
	raw, err := json.Marshal(ev.Data)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}
