package telemetry

import (
	"context"
	"testing"
	"time"

	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/jtarchie/steps/internal/events"
)

func recordRun(t *testing.T, published ...events.Event) map[string]sdktrace.ReadOnlySpan {
	t.Helper()

	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	tracer, err := newRunTracer(provider.Tracer("test"), metricnoop.NewMeterProvider().Meter("test"))
	if err != nil {
		t.Fatal(err)
	}

	at := time.Now()

	for i, event := range published {
		event.RunID = "r1"
		event.At = at.Add(time.Duration(i) * time.Millisecond)
		tracer.Event(context.Background(), event)
	}

	spans := map[string]sdktrace.ReadOnlySpan{}
	for _, stub := range exporter.GetSpans().Snapshots() {
		spans[stub.Name()] = stub
	}

	return spans
}

func attr(span sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}

	return ""
}

// A chain skip publishes only its skip, so its span is drawn whole from that one event, under the run.
func TestAChainSkipIsASpanOfItsOwn(t *testing.T) {
	spans := recordRun(t,
		events.Event{Type: events.TypeJobStarted, Job: "build"},
		events.Event{Type: events.TypeStepSkipped, StepID: 1, StepKind: "task", StepName: "unchanged", Status: "skipped", Text: "replayed from cache"},
		events.Event{Type: events.TypeJobFinished, Job: "build", Status: "succeeded"},
	)

	skipped, ok := spans["task unchanged"]
	if !ok {
		t.Fatalf("no span for the skipped step among %v", spans)
	}

	if skipped.Parent().SpanID() != spans["RUN build"].SpanContext().SpanID() {
		t.Error("the skipped step is not under the run")
	}

	if attr(skipped, "cicd.pipeline.task.run.result") != "skip" || attr(skipped, "steps.skip.reason") != "replayed from cache" {
		t.Errorf("the skip does not say so: %v", skipped.Attributes())
	}
}

// A step its job cut off still ends, under the job's verdict, rather than leaking an open span.
func TestAStepOpenWhenItsJobEndsIsEnded(t *testing.T) {
	spans := recordRun(t,
		events.Event{Type: events.TypeJobStarted, Job: "build"},
		events.Event{Type: events.TypeStepStarted, StepID: 1, StepKind: "task", StepName: "wedged"},
		events.Event{Type: events.TypeJobFinished, Job: "build", Status: "aborted"},
	)

	if _, ok := spans["task wedged"]; !ok {
		t.Fatal("the cut-off step's span never ended")
	}

	if got := attr(spans["RUN build"], "cicd.pipeline.result"); got != "cancellation" {
		t.Errorf("an aborted run's result = %q, want cancellation", got)
	}
}
