package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/jtarchie/steps/internal/events"
)

// meteredTracer is a run tracer over an in-memory span exporter and a manual metric reader.
func meteredTracer(t *testing.T) (*runTracer, *tracetest.InMemoryExporter, *sdkmetric.ManualReader) {
	t.Helper()

	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	reader := sdkmetric.NewManualReader()
	meters := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = meters.Shutdown(context.Background()) })

	tracer, err := newRunTracer(provider.Tracer("test"), meters.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}

	return tracer, exporter, reader
}

func spanNamed(t *testing.T, exporter *tracetest.InMemoryExporter, name string) sdktrace.ReadOnlySpan {
	t.Helper()

	for _, stub := range exporter.GetSpans().Snapshots() {
		if stub.Name() == name {
			return stub
		}
	}

	t.Fatalf("no span named %q among %d", name, len(exporter.GetSpans()))

	return nil
}

func hasAttr(span sdktrace.ReadOnlySpan, key string) bool {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return true
		}
	}

	return false
}

func collected(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.Metrics {
	t.Helper()

	var rm metricdata.ResourceMetrics

	err := reader.Collect(context.Background(), &rm)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	var all []metricdata.Metrics
	for _, scope := range rm.ScopeMetrics {
		all = append(all, scope.Metrics...)
	}

	return all
}

func activeRuns(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()

	var total int64

	for _, m := range collected(t, reader) {
		if m.Name != "cicd.pipeline.run.active" {
			continue
		}

		sum, ok := m.Data.(metricdata.Sum[int64])
		if !ok {
			t.Fatalf("cicd.pipeline.run.active is %T, want a Sum[int64]", m.Data)
		}

		for _, point := range sum.DataPoints {
			total += point.Value
		}
	}

	return total
}

// A finished step's span names the hash it ran under and the worker it ran on, and says nothing about either when the event has none.
func TestAFinishedStepSpanCarriesItsHashAndWorkerOnlyWhenItHasThem(t *testing.T) {
	t.Parallel()

	tracer, exporter, _ := meteredTracer(t)
	ctx := tracer.StartRun(events.WithRunID(context.Background(), "r1"), "r1", "build")

	at := time.Now()

	for i, step := range []events.Event{
		{StepID: 1, StepName: "placed", StepKind: "task", Hash: "h1", Worker: "gpu (ssh://box)"},
		{StepID: 2, StepName: "plain", StepKind: "task"},
	} {
		started, finished := step, step
		started.Type, finished.Type, finished.Status = events.TypeStepStarted, events.TypeStepFinished, events.StatusSucceeded
		started.RunID, finished.RunID = "r1", "r1"
		started.At, finished.At = at.Add(time.Duration(i)*time.Millisecond), at.Add(time.Duration(i+1)*time.Millisecond)

		tracer.Event(ctx, started)
		tracer.Event(ctx, finished)
	}

	placed := spanNamed(t, exporter, "task placed")
	if attr(placed, "steps.step.hash") != "h1" || attr(placed, "steps.worker") != "gpu (ssh://box)" {
		t.Errorf("the placed step's attributes = %v, want its hash and worker", placed.Attributes())
	}

	plain := spanNamed(t, exporter, "task plain")
	if hasAttr(plain, "steps.step.hash") || hasAttr(plain, "steps.worker") {
		t.Errorf("a step with no hash or worker carries %v, want neither attribute", plain.Attributes())
	}
}

// Only a failure or an error marks a span as an error; a pass, a skip and an abort do not.
func TestOnlyAFailedOrErroredStepIsMarkedAnError(t *testing.T) {
	t.Parallel()

	for status, want := range map[string]codes.Code{
		events.StatusFailed:    codes.Error,
		events.StatusErrored:   codes.Error,
		events.StatusSucceeded: codes.Unset,
		events.StatusSkipped:   codes.Unset,
		events.StatusAborted:   codes.Unset,
	} {
		tracer, exporter, _ := meteredTracer(t)
		ctx := tracer.StartRun(events.WithRunID(context.Background(), "r1"), "r1", "build")

		step := events.Event{RunID: "r1", StepID: 1, StepName: "s", StepKind: "task", At: time.Now()}
		started, finished := step, step
		started.Type, finished.Type, finished.Status, finished.Text = events.TypeStepStarted, events.TypeStepFinished, status, "why it ended"

		tracer.Event(ctx, started)
		tracer.Event(ctx, finished)

		got := spanNamed(t, exporter, "task s").Status()
		if got.Code != want {
			t.Errorf("a step that %s has span status %v, want %v", status, got.Code, want)
		}

		if want == codes.Error && got.Description != "why it ended" {
			t.Errorf("a step that %s has status description %q, want its text", status, got.Description)
		}
	}
}

// The identity a run was started under becomes steps.* attributes on its span, and its pipeline labels the run's metrics.
func TestARunsIdentityBecomesAttributesAndLabelsItsMetrics(t *testing.T) {
	t.Parallel()

	tracer, exporter, reader := meteredTracer(t)

	ctx := events.WithLogAttrs(events.WithRunID(context.Background(), "r1"), "pipeline", "demo", "resource", "repo")
	ctx = tracer.StartRun(ctx, "r1", "build")

	tracer.EndRun(ctx, "r1", events.StatusSucceeded, "")

	run := spanNamed(t, exporter, "RUN build")
	if attr(run, "steps.pipeline") != "demo" || attr(run, "steps.resource") != "repo" {
		t.Errorf("run span attributes = %v, want steps.pipeline=demo and steps.resource=repo", run.Attributes())
	}

	found := false

	for _, m := range collected(t, reader) {
		if m.Name != "cicd.pipeline.run.duration" {
			continue
		}

		histogram, ok := m.Data.(metricdata.Histogram[float64])
		if !ok || len(histogram.DataPoints) != 1 {
			t.Fatalf("cicd.pipeline.run.duration = %T with %d points, want one histogram point", m.Data, len(histogram.DataPoints))
		}

		if pipeline, _ := histogram.DataPoints[0].Attributes.Value("steps.pipeline"); pipeline.AsString() != "demo" {
			t.Errorf("the run's duration is labelled pipeline %q, want demo", pipeline.AsString())
		}

		found = true
	}

	if !found {
		t.Error("no cicd.pipeline.run.duration was recorded")
	}
}

// The active-runs gauge counts a run from its start and stops counting it when it ends.
func TestTheActiveRunsGaugeFallsBackToZero(t *testing.T) {
	t.Parallel()

	tracer, _, reader := meteredTracer(t)

	ctx := tracer.StartRun(events.WithRunID(context.Background(), "r1"), "r1", "build")
	if got := activeRuns(t, reader); got != 1 {
		t.Fatalf("active runs after a start = %d, want 1", got)
	}

	tracer.EndRun(ctx, "r1", events.StatusSucceeded, "")

	if got := activeRuns(t, reader); got != 0 {
		t.Errorf("active runs after the run ended = %d, want 0", got)
	}
}

func TestSecondsConvertsMilliseconds(t *testing.T) {
	t.Parallel()

	for ms, want := range map[int64]float64{0: 0, 250: 0.25, 1500: 1.5, 60000: 60} {
		if got := seconds(ms); got != want {
			t.Errorf("seconds(%d) = %v, want %v", ms, got, want)
		}
	}
}
