package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/runview"
)

// plainOutput is a context whose notes are rendered as the terminal renders them, and a func returning what was rendered so far.
func plainOutput(t *testing.T) (context.Context, func() string) {
	t.Helper()

	var out strings.Builder

	bus := events.New(nil)
	t.Cleanup(bus.Observe(runview.Plain(&out)))

	return events.WithBus(context.Background(), bus), out.String
}
