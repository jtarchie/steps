// Package telemetry is the one place steps meets the OpenTelemetry SDK, off unless OTEL_* asks: the docker and GCE clients already trace against the global provider, so installing one is all their calls need.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/jtarchie/steps/internal/events"
)

// shutdownBound caps the final flush: an unreachable collector must not hold a finished command's exit hostage.
const shutdownBound = 5 * time.Second

// Enabled answers whether the environment asks for traces. The exporter would default to localhost:4318 with nothing set, which is a dial on every run nobody asked for, so an exporter or an endpoint has to be named.
func Enabled(getenv func(string) string) bool {
	if strings.EqualFold(getenv("OTEL_SDK_DISABLED"), "true") {
		return false
	}

	if exporter := getenv("OTEL_TRACES_EXPORTER"); exporter != "" {
		return exporter != "none"
	}

	return getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// ponytail: OTLP over HTTP only, what Honeycomb and every collector take, so a setting that exporter would silently ignore is refused rather than read as a setup that works and sends nothing; add the grpc exporter if a backend ever needs it.
func unsupported(getenv func(string) string) error {
	if exporter := getenv("OTEL_TRACES_EXPORTER"); exporter != "" && exporter != "otlp" && exporter != "none" {
		return fmt.Errorf("telemetry: OTEL_TRACES_EXPORTER=%s is not supported, only otlp", exporter)
	}

	protocol := getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
	if protocol == "" {
		protocol = getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}

	if protocol != "" && protocol != "http/protobuf" {
		return fmt.Errorf("telemetry: OTLP protocol %s is not supported, only http/protobuf", protocol)
	}

	return nil
}

// Start returns the flush a command must run before it exits, a no-op when telemetry is off.
func Start(ctx context.Context, version string) (func(context.Context), error) {
	if !Enabled(os.Getenv) {
		return func(context.Context) {}, nil
	}

	err := unsupported(os.Getenv)
	if err != nil {
		return nil, err
	}

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry: %w", err)
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

	provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))

	otel.SetTracerProvider(provider)
	previous := events.SetTracer(newRunTracer(provider.Tracer("github.com/jtarchie/steps")))
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	return func(ctx context.Context) {
		// The likeliest reason this runs is a cancelled command, so the flush gets a context of its own.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownBound)
		defer cancel()

		events.SetTracer(previous)

		err := provider.Shutdown(ctx)
		if err != nil {
			slog.WarnContext(ctx, "telemetry.shutdown", "error", err)
		}
	}, nil
}

// Handler traces every request a server answers. echo stamps the matched route onto the request, which otelhttp reads back to name the span by route rather than by URL.
func Handler(next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, "steps")
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
