package events

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func maskHunter(text string) string { return strings.ReplaceAll(text, "hunter2", "***") }

// A run's secrets are masked in what a log line says, its message and its attributes alike, an error attribute included.
func TestScrubMasksLogLines(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	logger := slog.New(LogHandler(slog.NewTextHandler(&out, nil)))
	ctx := WithScrub(context.Background(), maskHunter)

	logger.ErrorContext(ctx, "poll hunter2", "error", errors.New("token hunter2 refused"), "detail", "hunter2")

	if strings.Contains(out.String(), "hunter2") || !strings.Contains(out.String(), "***") {
		t.Errorf("log line = %q, want hunter2 masked everywhere", out.String())
	}
}

// Every event published under the context is masked before any subscriber, observer or sink sees it.
func TestScrubMasksPublishedEvents(t *testing.T) {
	t.Parallel()

	bus := New(nil)

	var seen []Event

	bus.Observe(func(event Event) { seen = append(seen, event) })

	ctx := WithScrub(WithBus(context.Background(), bus), maskHunter)
	Publish(ctx, Event{Type: TypeStepFinished, Text: "failed: hunter2", Detail: "hunter2"})
	Note(ctx, NoteInfo, "saw hunter2")

	bus.Close()

	for _, event := range seen {
		if strings.Contains(event.Text+event.Detail, "hunter2") {
			t.Errorf("event %s carried the secret: %+v", event.Type, event)
		}
	}

	if len(seen) != 2 {
		t.Errorf("observed %d events, want 2", len(seen))
	}
}
