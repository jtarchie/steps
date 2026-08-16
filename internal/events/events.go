// Package events is the run-event bus: a stdlib-only leaf that carries what
// a run is doing, as it does it, from the packages executing a job to
// whatever is watching.
//
// It exists because a run's story was only ever told to a terminal. The
// merkle store answers "what content has succeeded" and nodes.result answers
// "what did this step produce", but neither answers "what is happening right
// now, in plan order" — the question a web UI, a progress display, or a live
// log tail is entirely made of.
//
// Two consumers, one stream. A subscriber (the web UI's SSE handler) sees
// events as they happen; a sink (the store) persists the same events so the
// run reads back identically after it ends. That is deliberate: a live view
// and a post-hoc view built from different sources drift, and the drift is
// always discovered in the middle of an incident.
//
// The bus travels on the context, like the pipeline's own resume and exec-log
// state, so publishing costs no signature changes in the packages that
// execute steps. A context with no bus makes every Publish a no-op, which is
// what `steps run` in a terminal gets.
package events

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Event types. A consumer switches on Type, so these are the vocabulary.
const (
	TypeJobStarted   = "job_started"
	TypeJobFinished  = "job_finished"
	TypeStepStarted  = "step_started"
	TypeStepFinished = "step_finished"
	TypeStepSkipped  = "step_skipped"
	// TypeStepOutput carries what a step printed. Published once when the
	// step ends rather than streamed line by line: a transcript wants the
	// output, and one bounded event per step costs a row instead of
	// thousands.
	TypeStepOutput = "step_output"
	// Agent conversation traffic, mirroring the persisted transcript's own
	// event vocabulary (see internal/agent/transcript.go) so a live view and
	// a stored transcript render through the same code path.
	TypeAgentText     = "agent_text"
	TypeAgentCall     = "agent_call"
	TypeAgentResult   = "agent_result"
	TypeAgentSubagent = "agent_subagent"
)

// Event is one thing that happened during a run.
//
// Flat rather than a type union: it is serialized to sqlite and to SSE, and
// a flat row keeps both ends free of a discriminated-union decoder for what
// is, in practice, a handful of optional strings.
type Event struct {
	// Seq orders events within a bus. Assigned on publish, monotonic, so a
	// subscriber that reconnects can ask for everything after what it has.
	Seq   int64     `json:"seq"`
	At    time.Time `json:"at"`
	Type  string    `json:"type"`
	RunID string    `json:"run_id"`
	Job   string    `json:"job"`
	// StepIndex is the plan index the event belongs to, -1 for job-level
	// events. A fan-out cell reports its parent's index and distinguishes
	// itself by StepName, matching how across: cells are already named.
	StepIndex int    `json:"step_index"`
	StepName  string `json:"step_name,omitempty"`
	StepKind  string `json:"step_kind,omitempty"`
	Status    string `json:"status,omitempty"`
	Hash      string `json:"hash,omitempty"`
	// Text carries model output, a log line, or an error message depending on
	// Type; Name and Detail carry a tool call's name and its arguments or
	// result. Kept as three generic fields rather than a per-type struct for
	// the reason the type itself is flat.
	Text       string `json:"text,omitempty"`
	Name       string `json:"name,omitempty"`
	Detail     string `json:"detail,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// subscriberBuffer bounds how far behind a subscriber may fall before its
// events start being dropped. A slow reader must never be able to stall the
// run producing the events — a browser tab that stopped reading is not a
// reason for a pipeline step to block.
const subscriberBuffer = 256

// sinkBuffer bounds the persistence queue. Same contract as above, one step
// stronger: the sink is drained by a single goroutine, which also serializes
// the sqlite writes rather than having concurrent steps contend for the
// write lock.
const sinkBuffer = 1024

// Bus fans one run's events out to live subscribers and, optionally, to a
// persistence sink. The zero value is not usable; call New.
type Bus struct {
	mu     sync.Mutex
	subs   map[int64]chan Event
	nextID int64
	seq    atomic.Int64

	sink     chan Event
	sinkDone chan struct{}
	// closed guards against publishing to a sink Close has already closed.
	// Both are read and written under mu.
	closed bool
}

// New returns a bus. When sink is non-nil it is called for every published
// event, in order, on a single goroutine — the caller does not need its own
// locking. Close stops that goroutine.
func New(sink func(Event)) *Bus {
	bus := &Bus{subs: map[int64]chan Event{}}

	if sink != nil {
		// The goroutine ranges over its OWN copy of the channel, never over
		// bus.sink: Close nils that field before closing the channel, and a
		// `range bus.sink` evaluated after the nil would range over nil and
		// block forever, leaving Close waiting on a sinkDone that never comes.
		queue := make(chan Event, sinkBuffer)
		bus.sink = queue
		bus.sinkDone = make(chan struct{})

		go func() {
			defer close(bus.sinkDone)

			for event := range queue {
				sink(event)
			}
		}()
	}

	return bus
}

// Publish stamps an event and delivers it to every subscriber and the sink.
// It never blocks: a subscriber whose buffer is full misses the event rather
// than holding up the run.
func (b *Bus) Publish(event Event) {
	if b == nil {
		return
	}

	event.Seq = b.seq.Add(1)

	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}

	for _, ch := range b.subs {
		select {
		case ch <- event:
		default: // slow subscriber: drop rather than stall the run
		}
	}

	// Sent under the same lock Close takes, because the alternative is a
	// send on a channel Close is concurrently closing — a panic, not a lost
	// event. The send is still non-blocking, so holding the lock costs a
	// bounded push and never waits on the sink's writer.
	if b.sink != nil {
		select {
		case b.sink <- event:
		default: // sink backed up: the live view still got it
		}
	}
}

// Subscribe returns a channel of subsequent events and a cancel func that
// closes it. The caller must call cancel, or the bus keeps feeding a channel
// nobody reads.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	if b == nil {
		closed := make(chan Event)
		close(closed)

		return closed, func() {}
	}

	ch := make(chan Event, subscriberBuffer)

	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()

		if existing, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(existing)
		}
	}
}

// Close drains and stops the sink goroutine, and makes every later Publish a
// no-op. Subscribers are left alone — each owns its own cancel.
//
// Idempotent, and safe against concurrent publishers: a run still finishing
// when the process shuts down would otherwise be sending on the very channel
// this closes.
func (b *Bus) Close() {
	if b == nil {
		return
	}

	b.mu.Lock()

	if b.closed {
		b.mu.Unlock()

		return
	}

	b.closed = true
	sink := b.sink
	b.sink = nil
	b.mu.Unlock()

	if sink == nil {
		return
	}

	close(sink)
	<-b.sinkDone
}

// busKey is the context key for the bus. Unexported and of a private type,
// so nothing outside this package can collide with it.
type busKey struct{}

// WithBus returns a context carrying bus, so the packages that execute steps
// can publish without every call site growing a parameter.
func WithBus(ctx context.Context, bus *Bus) context.Context {
	return context.WithValue(ctx, busKey{}, bus)
}

// FromContext returns the bus carried by ctx, or nil. A nil *Bus is a valid
// receiver for every method here, so callers need not check.
func FromContext(ctx context.Context) *Bus {
	bus, _ := ctx.Value(busKey{}).(*Bus)

	return bus
}

// Publish publishes to whatever bus ctx carries. It is the form the
// execution packages use: one call, no nil checks, no-op off the web path.
func Publish(ctx context.Context, event Event) {
	FromContext(ctx).Publish(event)
}

// runIDKey is the context key for the current run's id.
type runIDKey struct{}

// WithRunID tags ctx with the run every subsequent event belongs to.
//
// The id lives here rather than in the package that mints it because the
// packages that need to STAMP it (agent, publishing a conversation turn) must
// not import the package that owns the plan walk. A stdlib-only leaf both
// already depend on is the one place they can meet.
func WithRunID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, runIDKey{}, id)
}

// RunID returns the run id carried by ctx, or "".
func RunID(ctx context.Context) string {
	id, _ := ctx.Value(runIDKey{}).(string)

	return id
}

// loggerKey is the context key for the run-scoped logger.
type loggerKey struct{}

// WithLogger returns a context carrying logger, so every package executing a
// step logs under the run/job/step it belongs to without that identity being
// threaded through its signatures.
//
// It lives beside WithRunID, and for the same reason: which run a line
// belongs to is known by the package that owns the plan walk, and needed by
// packages that must not import it. A stdlib-only leaf both already depend on
// is the one place they can meet — and this stays stdlib-only, since log/slog
// is stdlib.
//
// The alternative — a jobName/index parameter pair on every function that
// might log — makes a diagnostic concern dictate the shape of everything it
// touches, and spreads exactly as far as the logging does.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

// Logger returns the logger ctx carries, or slog's default. Never nil, so a
// call site reads Logger(ctx).Info(...) with no check of its own.
func Logger(ctx context.Context) *slog.Logger {
	logger, ok := ctx.Value(loggerKey{}).(*slog.Logger)
	if !ok {
		return slog.Default()
	}

	return logger
}
