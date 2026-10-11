package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestEnabledNeedsAnExporterOrAnEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"nothing set", nil, false},
		{"endpoint", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}, true},
		{"traces endpoint", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4318/v1/traces"}, true},
		{"exporter named", map[string]string{"OTEL_TRACES_EXPORTER": "otlp"}, true},
		{"none beats an endpoint", map[string]string{"OTEL_TRACES_EXPORTER": "none", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}, false},
		{"sdk disabled beats an endpoint", map[string]string{"OTEL_SDK_DISABLED": "TRUE", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}, false},
		{"unrelated otel var", map[string]string{"OTEL_SERVICE_NAME": "ci"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Enabled(func(key string) string { return tc.env[key] })
			if got != tc.want {
				t.Errorf("Enabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnsupportedRefusesWhatWouldExportNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		refused bool
	}{
		{"honeycomb", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://api.honeycomb.io", "OTEL_EXPORTER_OTLP_HEADERS": "x-honeycomb-team=key"}, false},
		{"otlp named", map[string]string{"OTEL_TRACES_EXPORTER": "otlp"}, false},
		{"http/protobuf named", map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf"}, false},
		{"console", map[string]string{"OTEL_TRACES_EXPORTER": "console"}, true},
		{"grpc", map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, true},
		{"grpc for traces only", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc"}, true},
		{"traces protocol overrides the general one", map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "http/protobuf"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := unsupported(func(key string) string { return tc.env[key] })
			if (err != nil) != tc.refused {
				t.Errorf("unsupported = %v, want refused=%v", err, tc.refused)
			}
		})
	}
}

func TestLogHandlerNamesTheSpanALineWasLoggedUnder(t *testing.T) {
	var out bytes.Buffer

	logger := slog.New(LogHandler(slog.NewTextHandler(&out, nil)))

	span := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a, 0x0b},
		SpanID:     trace.SpanID{0x0c},
		TraceFlags: trace.FlagsSampled,
	})

	logger.InfoContext(trace.ContextWithSpanContext(context.Background(), span), "traced")
	logger.InfoContext(context.Background(), "untraced")

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want two lines, got %q", out.String())
	}

	if !strings.Contains(lines[0], "trace_id="+span.TraceID().String()) || !strings.Contains(lines[0], "span_id="+span.SpanID().String()) {
		t.Errorf("a line under a span does not name it: %s", lines[0])
	}

	if strings.Contains(lines[1], "trace_id") {
		t.Errorf("a line under no span claims one: %s", lines[1])
	}
}
