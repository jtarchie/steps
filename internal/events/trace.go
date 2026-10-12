package events

import (
	"context"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"
)

// Tracer turns a run's lifecycle events into spans. It is an interface here so every package that runs a step reaches tracing through this leaf, and only the one package that implements it imports a tracing SDK.
type Tracer interface {
	// StartRun opens a run's span the moment the run begins, before it has published anything, so a run refused by validation or preflight is still a trace; work under the returned context is traced beneath it.
	StartRun(ctx context.Context, runID, job string) context.Context
	// EndRun closes a run's span with the status its job finished on, and anything of it still open.
	EndRun(ctx context.Context, runID, status, text string)
	// Event is told every event a run publishes, inline and in order, before it reaches the bus.
	Event(ctx context.Context, event Event)
	// Scope returns ctx under the span for a run's step (0 for the run itself), so work the step does is traced beneath it.
	Scope(ctx context.Context, runID string, stepID int64) context.Context
	// Traceparent is the W3C trace context ctx is under, or "".
	Traceparent(ctx context.Context) string
}

// ponytail: process-wide, like the tracer provider it fronts; a ctx-carried tracer would need every command's context to be built through one funnel first.
var tracer atomic.Pointer[Tracer]

// SetTracer installs t for every run in the process; nil removes it. The previous one is returned so an owner can put it back.
func SetTracer(t Tracer) Tracer {
	var prev *Tracer
	if t == nil {
		prev = tracer.Swap(nil)
	} else {
		prev = tracer.Swap(&t)
	}

	if prev == nil {
		return nil
	}

	return *prev
}

func currentTracer() Tracer {
	if t := tracer.Load(); t != nil {
		return *t
	}

	return nil
}

// Traceparent is the W3C trace context a command started under ctx should continue, or "" with no tracer installed.
func Traceparent(ctx context.Context) string {
	if t := currentTracer(); t != nil {
		return t.Traceparent(ctx)
	}

	return ""
}

// LogAttrs is what WithLogAttrs put on ctx, which is also a run's identity (pipeline, revision, run, job) to anything recording it.
func LogAttrs(ctx context.Context) []slog.Attr {
	attrs, _ := ctx.Value(logAttrsKey{}).([]slog.Attr)

	return slices.Clone(attrs)
}

// StartRun opens the run ctx names (WithRunID) as a span, when a tracer is installed.
func StartRun(ctx context.Context, job string) context.Context {
	if t := currentTracer(); t != nil {
		return t.StartRun(ctx, RunID(ctx), job)
	}

	return ctx
}

// EndRun closes what StartRun opened.
func EndRun(ctx context.Context, status, text string) {
	if t := currentTracer(); t != nil {
		t.EndRun(ctx, RunID(ctx), status, text)
	}
}

func traceEvent(ctx context.Context, event Event) Event {
	t := currentTracer()
	if t == nil {
		return event
	}

	// Stamped here rather than by the bus so the span and the recorded event agree on when it happened.
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}

	t.Event(ctx, event)

	return event
}

func traceScope(ctx context.Context, id int64) context.Context {
	if t := currentTracer(); t != nil {
		return t.Scope(ctx, RunID(ctx), id)
	}

	return ctx
}
