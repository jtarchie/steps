package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/runview"
)

// plainRun is ctx naming the renderer `steps run` names for a run drawn in plain lines, on ctx's stdout as it resolves here.
func plainRun(ctx context.Context) context.Context {
	return events.WithRenderer(ctx, runview.Plain(events.Stdout(ctx)))
}

// plainOutput is a context whose notes are rendered as the terminal renders them, and a func returning what was rendered so far.
func plainOutput(t *testing.T) (context.Context, func() string) {
	t.Helper()

	var out strings.Builder

	bus := events.New(nil)
	t.Cleanup(bus.Observe(runview.Plain(&out)))

	return events.WithBus(context.Background(), bus), out.String
}
