package telemetry

import (
	"context"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/jtarchie/steps/internal/events"
)

// runTracer draws a run as its display tree: the job's span is the root, and each step's span sits under the step whose id is its ParentStepID, which is the tree the web transcript draws from the same events.
type runTracer struct {
	tracer trace.Tracer

	mu sync.Mutex
	// Keyed by step 0 for the run's own span, which no step can collide with because pipeline mints step ids from 1.
	open map[stepKey]trace.Span
}

type stepKey struct {
	run  string
	step int64
}

func newRunTracer(tracer trace.Tracer) *runTracer {
	return &runTracer{tracer: tracer, open: map[stepKey]trace.Span{}}
}

func (t *runTracer) Event(ctx context.Context, event events.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()

	//kindswitch:ignore only the lifecycle is a span; notes, output and agent turns stay in the transcript
	switch event.Type {
	case events.TypeJobStarted:
		t.startJob(ctx, event)
	case events.TypeStepStarted:
		t.startStep(ctx, event)
	case events.TypeStepFinished, events.TypeStepSkipped:
		t.endStep(ctx, event)
	case events.TypeJobFinished:
		t.endJob(event)
	}
}

func (t *runTracer) startJob(ctx context.Context, event events.Event) {
	identity := events.LogAttrs(ctx)
	attrs := make([]attribute.KeyValue, 0, 3+len(identity))
	attrs = append(attrs,
		attribute.String("cicd.pipeline.action.name", "RUN"),
		attribute.String("cicd.pipeline.name", event.Job),
		attribute.String("cicd.pipeline.run.id", event.RunID),
	)

	for _, attr := range identity {
		attrs = append(attrs, attribute.String("steps."+attr.Key, attr.Value.String()))
	}

	_, span := t.tracer.Start(ctx, "RUN "+event.Job,
		trace.WithSpanKind(trace.SpanKindServer), trace.WithTimestamp(event.At), trace.WithAttributes(attrs...))

	t.open[stepKey{event.RunID, 0}] = span
}

func (t *runTracer) startStep(ctx context.Context, event events.Event) {
	_, span := t.tracer.Start(t.parent(ctx, event), stepSpanName(event),
		trace.WithTimestamp(event.At), trace.WithAttributes(
			attribute.String("cicd.pipeline.task.name", event.StepName),
			attribute.String("steps.step.kind", event.StepKind),
		))

	t.open[stepKey{event.RunID, event.StepID}] = span
}

// endStep closes a step's span, or draws a zero-length one for a step that never announced a start: a chain skip publishes only its skip.
func (t *runTracer) endStep(ctx context.Context, event events.Event) {
	key := stepKey{event.RunID, event.StepID}

	span, ok := t.open[key]
	if !ok {
		_, span = t.tracer.Start(t.parent(ctx, event), stepSpanName(event), trace.WithTimestamp(event.At),
			trace.WithAttributes(attribute.String("cicd.pipeline.task.name", event.StepName), attribute.String("steps.step.kind", event.StepKind)))
	}

	delete(t.open, key)

	result := resultOf(event.Status)
	span.SetAttributes(attribute.String("cicd.pipeline.task.run.result", result))

	if event.Hash != "" {
		span.SetAttributes(attribute.String("steps.step.hash", event.Hash))
	}

	if event.Worker != "" {
		span.SetAttributes(attribute.String("steps.worker", event.Worker))
	}

	if event.Type == events.TypeStepSkipped {
		span.SetAttributes(attribute.String("steps.skip.reason", event.Text))
	} else {
		failed(span, result, event.Text)
	}

	span.End(trace.WithTimestamp(event.At))
}

func (t *runTracer) endJob(event events.Event) {
	result := resultOf(event.Status)

	for key, span := range t.open {
		if key.run != event.RunID {
			continue
		}

		// A step still open when its job ends was cut off by it; the job's own status says how.
		if key.step == 0 {
			span.SetAttributes(attribute.String("cicd.pipeline.result", result))
			failed(span, result, event.Text)
		}

		span.End(trace.WithTimestamp(event.At))
		delete(t.open, key)
	}
}

// parent is the span of the step an event's step ran inside, the run's span at the top of the plan, or whatever ctx carries when neither is open.
func (t *runTracer) parent(ctx context.Context, event events.Event) context.Context {
	if span, ok := t.open[stepKey{event.RunID, event.ParentStepID}]; ok {
		return trace.ContextWithSpan(ctx, span)
	}

	if span, ok := t.open[stepKey{event.RunID, 0}]; ok {
		return trace.ContextWithSpan(ctx, span)
	}

	return ctx
}

func (t *runTracer) Scope(ctx context.Context, runID string, stepID int64) context.Context {
	t.mu.Lock()
	span, ok := t.open[stepKey{runID, stepID}]
	t.mu.Unlock()

	if !ok {
		return ctx
	}

	return trace.ContextWithSpan(ctx, span)
}

func (t *runTracer) Traceparent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)

	return carrier.Get("traceparent")
}

func stepSpanName(event events.Event) string {
	return strings.TrimSpace(event.StepKind + " " + event.StepName)
}

// resultOf maps a run status onto the CI/CD semantic conventions' result values.
func resultOf(status string) string {
	switch status {
	case "succeeded":
		return "success"
	case "failed":
		return "failure"
	case "errored":
		return "error"
	case "aborted":
		return "cancellation"
	case "skipped":
		return "skip"
	default:
		return status
	}
}

func failed(span trace.Span, result, text string) {
	if result == "failure" || result == "error" {
		span.SetStatus(codes.Error, text)
	}
}
