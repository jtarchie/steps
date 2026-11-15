// Package telemetry is the one place steps meets the OpenTelemetry SDK, off unless OTEL_* asks: the docker and GCE clients already trace against the global provider, so installing one is all their calls need.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo-otel/v5"
	"github.com/labstack/echo/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/jtarchie/steps/internal/events"
)

// shutdownBound caps the final flush: an unreachable collector must not hold a finished command's exit hostage.
const shutdownBound = 5 * time.Second

// Enabled answers whether the environment asks for a signal, TRACES or METRICS. The exporter would default to localhost:4318 with nothing set, which is a dial on every run nobody asked for, so an exporter or an endpoint has to be named.
func Enabled(getenv func(string) string, signal string) bool {
	if strings.EqualFold(getenv("OTEL_SDK_DISABLED"), "true") {
		return false
	}

	if exporter := getenv("OTEL_" + signal + "_EXPORTER"); exporter != "" {
		return exporter != "none"
	}

	return getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT") != ""
}

// ponytail: OTLP over HTTP only, what Honeycomb and every collector take, so a setting that exporter would silently ignore is refused rather than read as a setup that works and sends nothing; add the grpc exporters if a backend ever needs them.
func unsupported(getenv func(string) string, signal string) error {
	if exporter := getenv("OTEL_" + signal + "_EXPORTER"); exporter != "" && exporter != "otlp" && exporter != "none" {
		return fmt.Errorf("telemetry: OTEL_%s_EXPORTER=%s is not supported, only otlp", signal, exporter)
	}

	protocol := getenv("OTEL_EXPORTER_OTLP_" + signal + "_PROTOCOL")
	if protocol == "" {
		protocol = getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}

	if protocol != "" && protocol != "http/protobuf" {
		return fmt.Errorf("telemetry: OTLP protocol %s is not supported, only http/protobuf", protocol)
	}

	return nil
}

func unsupportedIf(on bool, signal string) error {
	if !on {
		return nil
	}

	return unsupported(os.Getenv, signal)
}

// Start returns the flush a command must run before it exits, a no-op when telemetry is off.
func Start(ctx context.Context, version string) (func(context.Context), error) {
	traces, metrics := Enabled(os.Getenv, "TRACES"), Enabled(os.Getenv, "METRICS")
	if !traces && !metrics {
		return func(context.Context) {}, nil
	}

	err := errors.Join(unsupportedIf(traces, "TRACES"), unsupportedIf(metrics, "METRICS"))
	if err != nil {
		return nil, err
	}

	// Attributes first so OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES, read after, override them.
	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", "steps"),
			attribute.String("service.version", version),
		),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
	}

	tracer, stopTraces, err := startTraces(ctx, res, traces)
	if err != nil {
		return nil, err
	}

	meter, stopMetrics, err := startMetrics(ctx, res, metrics)
	if err != nil {
		unwind(ctx, stopTraces)

		return nil, err
	}

	recorder, err := newRunTracer(tracer, meter)
	if err != nil {
		unwind(ctx, stopTraces, stopMetrics)

		return nil, err
	}

	// Never put back: otel's default handler forwards for good to the first one ever set (a sync.Once in its setter), so restoring it would still land here.
	warnings.reset()
	otel.SetErrorHandler(otel.ErrorHandlerFunc(warnings.handle))

	previous := events.SetTracer(recorder)

	return func(ctx context.Context) {
		// The likeliest reason this runs is a cancelled command, so the flush gets a context of its own.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownBound)
		defer cancel()

		events.SetTracer(previous)

		// Tagged here too: a collector that hangs makes Shutdown return a bare context error no exporter saw.
		err := tag("traces", stopTraces(ctx))
		if err != nil {
			warnings.handle(err)
		}

		err = tag("metrics", stopMetrics(ctx))
		if err != nil {
			warnings.handle(err)
		}
	}, nil
}

// repeatAfter is how long a signal that keeps failing stays quiet: a command shorter than it warns once, a daemon is told again while the failure lasts.
const repeatAfter = 10 * time.Minute

// signalError marks an error as one signal's exporter's, so the handler, which otel calls with nothing but the error, can tell traces from metrics from the rest of the SDK.
type signalError struct {
	signal string
	err    error
}

func (e signalError) Error() string { return e.err.Error() }
func (e signalError) Unwrap() error { return e.err }

func tag(signal string, err error) error {
	if err == nil {
		return nil
	}

	return signalError{signal: signal, err: err}
}

type tracesExporter struct{ sdktrace.SpanExporter }

func (e tracesExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	return tag("traces", e.SpanExporter.ExportSpans(ctx, spans))
}

type metricsExporter struct{ sdkmetric.Exporter }

func (e metricsExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	return tag("metrics", e.Exporter.Export(ctx, rm))
}

// warner is otel's error handler (#236): the SDK reports every batch it could not send and the final flush fails the same way, so each signal warns once, then again only after repeatAfter while it keeps failing.
type warner struct {
	now func() time.Time

	mu         sync.Mutex
	last       map[string]time.Time
	suppressed map[string]int
}

func newWarner(now func() time.Time) *warner {
	w := &warner{now: now}
	w.reset()

	return w
}

var warnings = newWarner(time.Now)

// reset is called by each Start, so every command gets warnings of its own.
func (w *warner) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.last = map[string]time.Time{}
	w.suppressed = map[string]int{}
}

func (w *warner) handle(err error) {
	signal, note := "otel", "an OpenTelemetry SDK error"

	var tagged signalError
	if errors.As(err, &tagged) {
		signal, note = tagged.signal, "telemetry is not reaching its collector"
	}

	w.mu.Lock()

	now := w.now()

	last, warned := w.last[signal]
	if warned && now.Sub(last) < repeatAfter {
		w.suppressed[signal]++
		w.mu.Unlock()

		return
	}

	suppressed := w.suppressed[signal]
	w.last[signal] = now
	w.suppressed[signal] = 0
	w.mu.Unlock()

	slog.Warn("telemetry.error", "signal", signal, "error", err, "note", note, "suppressed", suppressed, "repeats_after", repeatAfter)
}

// unwind shuts down what Start already installed when a later step fails, under a context of its own because the caller's may be the reason Start failed.
func unwind(ctx context.Context, stops ...func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownBound)
	defer cancel()

	for _, stop := range stops {
		_ = stop(ctx)
	}
}

func noShutdown(context.Context) error { return nil }

func startTraces(ctx context.Context, res *resource.Resource, on bool) (trace.Tracer, func(context.Context) error, error) {
	if !on {
		return tracenoop.NewTracerProvider().Tracer(""), noShutdown, nil
	}

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry: %w", err)
	}

	provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(tracesExporter{exporter}), sdktrace.WithResource(res))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	// Put back, as events.SetTracer is: otelhttp resolves the global provider per request, so a command ending in-process would otherwise leave another still running tracing into a provider that has shut down.
	return provider.Tracer(scope), func(ctx context.Context) error {
		otel.SetTracerProvider(previous)

		return provider.Shutdown(ctx)
	}, nil
}

func startMetrics(ctx context.Context, res *resource.Resource, on bool) (metric.Meter, func(context.Context) error, error) {
	if !on {
		return metricnoop.NewMeterProvider().Meter(""), noShutdown, nil
	}

	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricsExporter{exporter})), sdkmetric.WithResource(res))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)

	return provider.Meter(scope), func(ctx context.Context) error {
		otel.SetMeterProvider(previous)

		return provider.Shutdown(ctx)
	}, nil
}

// scope names this program as the instrumentation behind every span and metric it records itself.
const scope = "github.com/jtarchie/steps"

// ObserveQueue reports pending builds per pipeline as steps.queue.pending, asking pending at each export; the returned func stops it, and must run before whatever pending reads from is closed.
func ObserveQueue(pending func(context.Context) map[string]int64) func() {
	meter := otel.Meter(scope)

	gauge, err := meter.Int64ObservableGauge("steps.queue.pending", metric.WithUnit("{build}"),
		metric.WithDescription("Builds queued and not yet started, per pipeline"))
	if err != nil {
		otel.Handle(err)

		return func() {}
	}

	registration, err := meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		for pipeline, count := range pending(ctx) {
			observer.ObserveInt64(gauge, count, metric.WithAttributes(attribute.String("steps.pipeline", pipeline)))
		}

		return nil
	}, gauge)
	if err != nil {
		otel.Handle(err)

		return func() {}
	}

	return func() { _ = registration.Unregister() }
}

// Middleware traces every request an echo server answers but those to untraced paths, which a platform polls every few seconds. It is echo's own OpenTelemetry middleware, which names the span by the matched route rather than by URL.
func Middleware(untraced ...string) echo.MiddlewareFunc {
	return echootel.NewMiddlewareWithConfig(echootel.Config{
		Skipper: func(c *echo.Context) bool { return slices.Contains(untraced, c.Request().URL.Path) },
	})
}

// Transport traces every request a client makes and carries the trace context to the server.
func Transport(base http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(base)
}

// LogHandler adds trace_id and span_id to a record logged under a recording span, so a log line can be found from its trace and back.
func LogHandler(inner slog.Handler) slog.Handler {
	return traceHandler{inner: inner}
}

type traceHandler struct {
	inner slog.Handler
}

func (h traceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx != nil {
		if span := trace.SpanContextFromContext(ctx); span.IsValid() {
			r = r.Clone()
			r.AddAttrs(slog.String("trace_id", span.TraceID().String()), slog.String("span_id", span.SpanID().String()))
		}
	}

	return h.inner.Handle(ctx, r) //nolint:wrapcheck // a pass-through: the wrapped handler's error is the answer
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{inner: h.inner.WithGroup(name)}
}
