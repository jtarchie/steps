package scheduler

import (
	"bytes"
	"log/slog"
	"os"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()

	err := os.WriteFile(path, []byte(body), 0o600)
	if err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// captureLogs swaps the process's logger, so a caller must not run in parallel.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return &buf
}
