package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/jtarchie/steps/internal/events"
)

// A Start with telemetry on installs the run tracer for every run in the process, and the flush it returns takes it out again. The exporters are built lazily, so an endpoint nobody listens on still starts.
func TestStartInstallsTheRunTracerAndItsFlushRemovesIt(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")

	flush, err := Start(context.Background(), "test")
	if err != nil {
		t.Fatalf("Start with telemetry on = %v, want it started", err)
	}

	installed := events.SetTracer(nil)
	if installed == nil {
		t.Fatal("Start installed no run tracer")
	}

	events.SetTracer(installed)

	flush(context.Background())

	if left := events.SetTracer(nil); left != nil {
		t.Error("the flush left the run tracer installed")
	}
}

// With telemetry off Start does nothing and installs nothing.
func TestStartWithTelemetryOffInstallsNothing(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")

	flush, err := Start(context.Background(), "test")
	if err != nil {
		t.Fatalf("Start with telemetry off = %v", err)
	}

	if installed := events.SetTracer(nil); installed != nil {
		t.Error("Start with telemetry off installed a run tracer")
	}

	flush(context.Background())
}

// A traces exporter that cannot be built (plain-http endpoint with a CA configured) fails Start and installs nothing.
func TestStartRefusesWhenTheTracesExporterCannotBeBuilt(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE", writeCA(t))

	_, err := Start(context.Background(), "test")
	if err == nil {
		t.Fatal("Start succeeded with an http endpoint and a CA for traces")
	}

	if installed := events.SetTracer(nil); installed != nil {
		t.Error("a failed Start left a run tracer installed")
	}
}

// ObserveQueue reports each pipeline's pending builds as steps.queue.pending at every export, and stops when the func it returns runs.
func TestObserveQueueReportsPendingPerPipelineUntilStopped(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()

	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(previous)

		_ = provider.Shutdown(context.Background())
	})

	stop := ObserveQueue(func(context.Context) map[string]int64 { return map[string]int64{"app": 3, "other": 0} })

	pending := func() map[string]int64 {
		got := map[string]int64{}

		for _, m := range collected(t, reader) {
			if m.Name != "steps.queue.pending" {
				continue
			}

			gauge, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("steps.queue.pending is %T, want a Gauge[int64]", m.Data)
			}

			for _, point := range gauge.DataPoints {
				name, _ := point.Attributes.Value("steps.pipeline")
				got[name.AsString()] = point.Value
			}
		}

		return got
	}

	if got := pending(); got["app"] != 3 || len(got) != 2 {
		t.Errorf("pending = %v, want app=3 and other=0", got)
	}

	stop()

	if got := pending(); len(got) != 0 {
		t.Errorf("pending after stop = %v, want nothing reported", got)
	}
}
