package trigger

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
)

type checkTracer struct {
	events.Tracer

	started []string
	ended   []error
}

type checkSpanKey struct{}

func (c *checkTracer) StartCheck(ctx context.Context, resource string) (context.Context, func(error)) {
	c.started = append(c.started, resource)

	return context.WithValue(ctx, checkSpanKey{}, "00-check-of-"+resource), func(err error) { c.ended = append(c.ended, err) }
}

func (c *checkTracer) Carry(context.Context) func(context.Context) context.Context {
	return func(ctx context.Context) context.Context { return ctx }
}

func (c *checkTracer) Traceparent(ctx context.Context) string {
	parent, _ := ctx.Value(checkSpanKey{}).(string)

	return parent
}

// Not parallel: the tracer is process-wide.
func traceChecks(t *testing.T) *checkTracer {
	t.Helper()

	tracer := &checkTracer{}
	previous := events.SetTracer(tracer)
	t.Cleanup(func() { events.SetTracer(previous) })

	return tracer
}

// A check runs outside any run, so without a span of its own its container calls were roots with nothing above them: this carries the check's span across into the command the check runs.
func TestAPolledCheckRunsUnderItsOwnSpan(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "traceparent")
	cfg := loadConfig(t, dir, strings.Replace(pollFeed, "cat VERSIONS", `printf %s "$TRACEPARENT" > `+seen+` && echo '[{"n":"1"}]'`, 1))
	tracer := traceChecks(t)

	_, err := pollOnce(t.Context(), cfg, mustOpenStore(t, dir))
	if err != nil {
		t.Fatal(err)
	}

	if len(tracer.started) != 1 || tracer.started[0] != "items" {
		t.Fatalf("checks started = %v, want one for items", tracer.started)
	}

	if len(tracer.ended) != 1 || tracer.ended[0] != nil {
		t.Errorf("checks ended = %v, want one that succeeded", tracer.ended)
	}

	got, _ := os.ReadFile(seen) //nolint:gosec // a t.TempDir()-scoped file the check under test wrote
	if string(got) != "00-check-of-items" {
		t.Errorf("the check's command ran under %q, want the check's span", got)
	}
}

func TestAFailedCheckEndsItsSpanWithTheError(t *testing.T) {
	dir := t.TempDir()
	cfg := loadConfig(t, dir, strings.Replace(pollFeed, "cat VERSIONS", "echo no feed >&2; exit 3", 1))
	tracer := traceChecks(t)

	_, _ = pollOnce(t.Context(), cfg, mustOpenStore(t, dir))

	if len(tracer.ended) != 1 || tracer.ended[0] == nil || !strings.Contains(tracer.ended[0].Error(), "items") {
		t.Errorf("checks ended = %v, want the failure naming items", tracer.ended)
	}
}
