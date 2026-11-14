package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
)

// A collector nobody is running is one warning, not a line per export and another at shutdown (#236).
func TestAnUnreachableCollectorIsOneWarning(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")

	var out bytes.Buffer

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, nil)))

	defer slog.SetDefault(previous)

	flush, err := Start(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		_, span := otel.Tracer("test").Start(context.Background(), "work")
		span.End()

		otel.Handle(context.DeadlineExceeded)
	}

	flush(context.Background())

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], "telemetry.export_failed") {
		t.Errorf("want exactly one telemetry.export_failed warning, got %d lines:\n%s", len(lines), out.String())
	}

	out.Reset()

	flush, err = Start(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	flush(context.Background())

	if !strings.Contains(out.String(), "telemetry.export_failed") {
		t.Errorf("a second command in this process did not get its own warning:\n%s", out.String())
	}
}
