package telemetry

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/jtarchie/steps/internal/events"
)

// runTracer draws a run as its display tree, and measures it: the job's span is the root, and each step's span sits under the step whose id is its ParentStepID, which is the tree the web transcript draws from the same events.
type runTracer struct {
	tracer trace.Tracer

	runDuration  metric.Float64Histogram
	runsActive   metric.Int64UpDownCounter
	stepDuration metric.Float64Histogram

	mu sync.Mutex
	// Keyed by step 0 for the run's own span, which no step can collide with because pipeline mints step ids from 1.
	open map[stepKey]trace.Span
	// runs holds what a run's metrics need at its end, read off its start, since only the job's context carries the pipeline's name.
	runs map[string]runInfo
}

type runInfo struct {
	pipeline string
	job      string
	started  time.Time
}

type stepKey struct {
	run  string
	step int64
}

// Run metrics use the CI/CD conventions' names; a step has no conventional metric, hence steps.step.duration.
func newRunTracer(tracer trace.Tracer, meter metric.Meter) (*runTracer, error) {
	runDuration, err := meter.Float64Histogram("cicd.pipeline.run.duration", metric.WithUnit("s"),
		metric.WithDescription("How long a run of a job took, by result"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}

	runsActive, err := meter.Int64UpDownCounter("cicd.pipeline.run.active", metric.WithUnit("{run}"),
		metric.WithDescription("Runs executing now"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}

	stepDuration, err := meter.Float64Histogram("steps.step.duration", metric.WithUnit("s"),
		metric.WithDescription("How long a step took, by kind and result; a skip counts at zero, with its reason"))
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}

	return &runTracer{
		tracer:       tracer,
		runDuration:  runDuration,
		runsActive:   runsActive,
		stepDuration: stepDuration,
		open:         map[stepKey]trace.Span{},
		runs:         map[string]runInfo{},
	}, nil
}

func (t *runTracer) Event(ctx context.Context, event events.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()

	//kindswitch:ignore steps and their notes are the trace; the run's own span is StartRun/EndRun's, and output and agent turns stay in the transcript
	switch event.Type {
	case events.TypeStepStarted:
		t.startStep(ctx, event)
	case events.TypeStepFinished, events.TypeStepSkipped:
		t.endStep(ctx, event)
	case events.TypeStepNote:
		t.note(event)
	}
}

// note puts what a step said on its span, or the run's for a note made outside any step.
func (t *runTracer) note(event events.Event) {
	span, ok := t.open[stepKey{event.RunID, event.StepID}]
	if !ok {
		return
	}

	span.AddEvent(event.Text, trace.WithTimestamp(event.At), trace.WithAttributes(attribute.String("steps.note.level", event.Status)))
}

func (t *runTracer) StartRun(ctx context.Context, runID, job string) context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()

	info := runInfo{job: job, started: time.Now()}
	identity := events.LogAttrs(ctx)
	attrs := make([]attribute.KeyValue, 0, 3+len(identity))
	attrs = append(attrs,
		attribute.String("cicd.pipeline.action.name", "RUN"),
		attribute.String("cicd.pipeline.name", job),
		attribute.String("cicd.pipeline.run.id", runID),
	)

	for _, attr := range identity {
		attrs = append(attrs, attribute.String("steps."+attr.Key, attr.Value.String()))

		if attr.Key == "pipeline" {
			info.pipeline = attr.Value.String()
		}
	}

	t.runs[runID] = info
	t.runsActive.Add(ctx, 1, metric.WithAttributes(info.attrs()...))

	ctx, span := t.tracer.Start(ctx, "RUN "+job,
		trace.WithSpanKind(trace.SpanKindServer), trace.WithTimestamp(info.started), trace.WithAttributes(attrs...))

	t.open[stepKey{runID, 0}] = span

	return ctx
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

	measured := []attribute.KeyValue{
		attribute.String("steps.pipeline", t.runs[event.RunID].pipeline),
		attribute.String("steps.step.kind", event.StepKind),
		attribute.String("cicd.pipeline.task.run.result", result),
	}

	if event.Type == events.TypeStepSkipped {
		span.SetAttributes(attribute.String("steps.skip.reason", event.Text))
		measured = append(measured, attribute.String("steps.skip.reason", event.Text))
	} else {
		failed(span, result, event.Text)
	}

	t.stepDuration.Record(ctx, seconds(event.DurationMS), metric.WithAttributes(measured...))

	span.End(trace.WithTimestamp(event.At))
}

func (t *runTracer) EndRun(ctx context.Context, runID, status, text string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	info, ok := t.runs[runID]
	if !ok {
		return
	}

	delete(t.runs, runID)

	result := resultOf(status)
	ended := time.Now()

	t.runsActive.Add(ctx, -1, metric.WithAttributes(info.attrs()...))
	t.runDuration.Record(ctx, ended.Sub(info.started).Seconds(),
		metric.WithAttributes(append(info.attrs(), attribute.String("cicd.pipeline.result", result))...))

	for key, span := range t.open {
		if key.run != runID {
			continue
		}

		// A step still open when its run ends was cut off by it; the run's own status says how.
		if key.step == 0 {
			span.SetAttributes(attribute.String("cicd.pipeline.result", result))
			failed(span, result, text)
		}

		span.End(trace.WithTimestamp(ended))
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

// attrs is a run's identity as a metric sees it: never the run id, whose every value is new.
func (r runInfo) attrs() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("steps.pipeline", r.pipeline),
		attribute.String("cicd.pipeline.name", r.job),
	}
}

func seconds(ms int64) float64 {
	return float64(ms) / 1000
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
