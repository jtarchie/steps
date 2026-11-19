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

// A structured attribute is masked as the handler would print it: a map, a slice, raw bytes, and anything nested in a group.
func TestScrubMasksStructuredAttrs(t *testing.T) {
	t.Parallel()

	for name, attr := range map[string]slog.Attr{
		"map":   slog.Any("args", map[string]any{"token": "hunter2"}),
		"slice": slog.Any("argv", []string{"--password", "hunter2"}),
		"bytes": slog.Any("body", []byte("token=hunter2")),
		"group": slog.Group("req", slog.String("auth", "hunter2"), slog.Group("inner", slog.Any("args", map[string]string{"k": "hunter2"}))),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for handler, newHandler := range map[string]func(*bytes.Buffer) slog.Handler{
				"text": func(out *bytes.Buffer) slog.Handler { return slog.NewTextHandler(out, nil) },
				"json": func(out *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(out, nil) },
			} {
				var out bytes.Buffer

				slog.New(LogHandler(newHandler(&out))).LogAttrs(WithScrub(context.Background(), maskHunter), slog.LevelInfo, "call", attr)

				if strings.Contains(out.String(), "hunter2") || !strings.Contains(out.String(), "***") {
					t.Errorf("%s log line = %q, want hunter2 masked", handler, out.String())
				}
			}
		})
	}
}

// A structured value with nothing to mask keeps its type, so a JSON handler still writes it as an object rather than a string.
func TestScrubKeepsCleanStructuredAttrs(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	slog.New(LogHandler(slog.NewJSONHandler(&out, nil))).InfoContext(WithScrub(context.Background(), maskHunter), "call", "args", map[string]any{"path": "a.go"})

	if !strings.Contains(out.String(), `"args":{"path":"a.go"}`) {
		t.Errorf("log line = %q, want args kept as a JSON object", out.String())
	}
}
