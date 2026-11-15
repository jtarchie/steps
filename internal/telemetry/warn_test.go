package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()

	var out bytes.Buffer

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return &out
}

func logLines(out *bytes.Buffer) []string {
	if out.Len() == 0 {
		return nil
	}

	return strings.Split(strings.TrimSpace(out.String()), "\n")
}

// A collector nobody is running is one warning per signal, from the SDK's own failed exports, not a line per export and another at shutdown (#236).
func TestAnUnreachableCollectorIsOneWarningPerSignal(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")

	out := captureLog(t)

	flush, err := Start(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		_, span := otel.Tracer("test").Start(context.Background(), "work")
		span.End()
	}

	flush(context.Background())

	lines := logLines(out)
	for _, signal := range []string{"traces", "metrics"} {
		count := countWarnings(lines, signal)
		if count != 1 {
			t.Errorf("want one telemetry.error warning for %s, got %d:\n%s", signal, count, out.String())
		}
	}

	if len(lines) != 2 {
		t.Errorf("want two lines, got %d:\n%s", len(lines), out.String())
	}

	out.Reset()

	flush, err = Start(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	flush(context.Background())

	if !strings.Contains(out.String(), "signal=metrics") {
		t.Errorf("a second command in this process did not get its own warning:\n%s", out.String())
	}
}

// countWarnings counts the telemetry.error warnings in lines that name signal.
func countWarnings(lines []string, signal string) int {
	count := 0

	for _, line := range lines {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "msg=telemetry.error") && strings.Contains(line, "signal="+signal+" ") {
			count++
		}
	}

	return count
}

// linesContain reports whether lines is exactly as long as wants and each line contains its want.
func linesContain(lines []string, wants ...string) bool {
	if len(lines) != len(wants) {
		return false
	}

	for i, want := range wants {
		if !strings.Contains(lines[i], want) {
			return false
		}
	}

	return true
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p) //nolint:wrapcheck // a test double over bytes.Buffer
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// A daemon's exports fail long before its flush, from the SDK's background goroutines, and each is still named by its signal.
func TestExportsFailingMidRunAreNamedBySignal(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "10")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "10")

	var out lockedBuffer

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, nil)))

	defer slog.SetDefault(previous)

	flush, err := Start(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	defer flush(context.Background())

	_, span := otel.Tracer("test").Start(context.Background(), "work")
	span.End()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		logged := out.String()
		if strings.Contains(logged, "signal=traces") && strings.Contains(logged, "signal=metrics") {
			if strings.Contains(logged, "signal=otel") {
				t.Errorf("a failed export was warned as an SDK error:\n%s", logged)
			}

			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Errorf("want both signals warned before the flush:\n%s", out.String())
}

// A collector that hangs makes the final flush give up on its own bound with a bare context error, which is still that signal's collector failing, not an SDK error.
func TestAHangingCollectorAtFlushIsTheSignalsWarning(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))

	defer server.Close()
	defer close(release)

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	t.Setenv("OTEL_METRICS_EXPORTER", "none")

	out := captureLog(t)

	flush, err := Start(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	_, span := otel.Tracer("test").Start(context.Background(), "work")
	span.End()

	flush(context.Background())

	if !strings.Contains(out.String(), "signal=traces") || strings.Contains(out.String(), "signal=otel") {
		t.Errorf("want the hung flush warned as traces:\n%s", out.String())
	}
}

// A daemon outlives one failure: the warning comes back after a quiet interval while failures continue, and each signal and non-export errors keep their own (#236 follow-up).
func TestErrorWarningsRepeatPerSignalAfterAQuietInterval(t *testing.T) {
	out := captureLog(t)

	clock := time.Unix(0, 0)
	w := newWarner(func() time.Time { return clock })

	traces := signalError{signal: "traces", err: errors.New("traces down")}
	metrics := signalError{signal: "metrics", err: errors.New("metrics down")}

	w.handle(traces)
	w.handle(traces)
	w.handle(metrics)
	w.handle(errors.New("bad resource attribute"))

	lines := logLines(out)
	if !linesContain(lines, "signal=traces", "signal=metrics", "signal=otel") || strings.Contains(lines[2], "collector") {
		t.Fatalf("want one line each for traces, metrics and an untagged error:\n%s", out.String())
	}

	out.Reset()
	clock = clock.Add(repeatAfter - time.Second)
	w.handle(traces)

	if out.Len() != 0 {
		t.Fatalf("warned again inside the quiet interval:\n%s", out.String())
	}

	clock = clock.Add(time.Second)
	w.handle(traces)

	lines = logLines(out)
	if !linesContain(lines, "signal=traces") || !strings.Contains(lines[0], "suppressed=2") {
		t.Fatalf("want traces warned again, counting the two it held back:\n%s", out.String())
	}

	out.Reset()
	w.reset()
	w.handle(traces)

	if !strings.Contains(out.String(), "signal=traces") {
		t.Errorf("reset did not give the next command its own warning:\n%s", out.String())
	}
}
