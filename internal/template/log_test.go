package template

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// The data a command renders against is a resource's source:, credentials included; debug logging must not copy it out.
func TestRenderDoesNotLogTheCredentialsItRenders(t *testing.T) {
	var logs bytes.Buffer

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	_, err := Render("git clone {{ .source.uri }}", map[string]any{
		"source": map[string]any{"uri": "https://u:hunter2secret@example.com/r.git"}, //nolint:gosec // a fake credential, the input under test
	})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(logs.String(), "hunter2secret") {
		t.Errorf("debug logs carry the credential: %s", logs.String())
	}

	if !strings.Contains(logs.String(), "template.render") {
		t.Errorf("logs = %q, want the render still traced", logs.String())
	}
}
