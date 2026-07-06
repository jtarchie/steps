// Package journal is the run's source of truth: an append-only event log.
// The run context is a fold over handler_finished events — resume is
// fold + continue, never replay of side effects.
package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// EventType enumerates journal event types.
type EventType string

const (
	// RunEnqueued records a durably-queued run: the inputs are pinned so a
	// dispatcher can start it later (even after a serve restart), but the loop
	// has not run. RunStarted is appended at dispatch — never here — so the
	// run's timeout baseline begins when it executes, not when it queues.
	RunEnqueued EventType = "run_enqueued"
	RunStarted  EventType = "run_started"
	// ForkStarted records a parallel fork's intent BEFORE any branch child run
	// exists: it pins the child run IDs (and their labels/entries) so a crash
	// mid-fork reattaches to the same children on resume instead of spawning a
	// new set. Written by the fork state; the parent journal stays single-cursor
	// (one state_entered + one handler_finished bracket the whole fork).
	ForkStarted     EventType = "fork_started"
	StateEntered    EventType = "state_entered"
	HandlerFinished EventType = "handler_finished"
	HandlerFailed   EventType = "handler_failed"
	RetryScheduled  EventType = "retry_scheduled"
	TransitionFired EventType = "transition_fired"
	RunParked       EventType = "run_parked"
	RunResumed      EventType = "run_resumed"
	RunFinished     EventType = "run_finished"
)

// ChildRef identifies one parallel branch's child run, pinned in a fork_started
// event so resume reattaches to the same child instead of spawning a new one.
type ChildRef struct {
	Label string `json:"label"`
	RunID string `json:"run_id"`
	Entry string `json:"entry"`
}

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
	ID      string
	Machine string
	Hash    string
	// Source is the machine's JS pinned at start; Assets are its include()d
	// files. Resume re-evaluates these bytes — never the filesystem.
	Source       []byte
	Assets       map[string]string
	Status       string // queued | running | parked | done | failed
	CurrentState string
	// ParentRunID is set on a parallel branch's child run — the fork state's
	// run. Empty for top-level runs. Lets listings hide children and the
	// run-detail view nest branch sub-runs.
	ParentRunID string
	Created     time.Time
	Updated     time.Time
}

// Run statuses.
const (
	// StatusQueued: durably enqueued, awaiting a dispatch slot. The run row
	// and its inputs exist, but the loop has not run.
	StatusQueued  = "queued"
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
	// ListRunsByStatus returns runs with the given status, oldest first (FIFO)
	// — the dispatcher drains queued runs in enqueue order.
	ListRunsByStatus(ctx context.Context, status string) ([]*Run, error)
	// ListChildRuns returns a parent run's parallel branch children, oldest
	// first — the run-detail view nests their sub-timelines.
	ListChildRuns(ctx context.Context, parentID string) ([]*Run, error)
	// Append assigns and returns the next sequence number.
	Append(ctx context.Context, ev *Event) (int, error)
	Events(ctx context.Context, runID string) ([]*Event, error)
	// Memoization: cache state outputs keyed by a hash of their rendered
	// input, across runs. Byte-identical input -> replayed output.
	MemoGet(ctx context.Context, key string) (map[string]any, bool, error)
	MemoPut(ctx context.Context, key string, output map[string]any) error
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
func (u *Usage) Total() int { return u.InputTokens + u.OutputTokens }

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
		return fmt.Errorf("encoding event data: %w", err)
	}
	err = json.Unmarshal(raw, into)
	if err != nil {
		return fmt.Errorf("decoding event data: %w", err)
	}
	return nil
}
