package cli

import (
	"log/slog"
	"os"
	"testing"
)

// Leaves slog.Default() alone: every Run() elsewhere in this package installs a new global logger, so asserting on it races whichever Run() finished last.
func TestParseLogLevel(t *testing.T) {
	t.Parallel()

	cases := map[string]slog.Level{
		"debug":        slog.LevelDebug,
		"info":         slog.LevelInfo,
		"warn":         slog.LevelWarn,
		"error":        slog.LevelError,
		"":             slog.LevelInfo,
		"unrecognized": slog.LevelInfo,
	}

	for level, want := range cases {
		if got := parseLogLevel(level); got != want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", level, got, want)
		}
	}
}

// Not t.Parallel(): it swaps os.Stderr and sets NO_COLOR. /dev/null stands in for a terminal because both are character devices, which is all wantNoColor asks.
func TestColorIsForATerminalThatDidNotOptOut(t *testing.T) {
	tty, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}

	piped, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}

	orig := os.Stderr

	t.Cleanup(func() {
		os.Stderr = orig
		_ = tty.Close()
		_ = piped.Close()
	})

	t.Setenv("NO_COLOR", "")

	os.Stderr = tty
	if wantNoColor() {
		t.Error("a terminal with NO_COLOR unset got no color")
	}

	t.Setenv("NO_COLOR", "1")

	if !wantNoColor() {
		t.Error("NO_COLOR=1 on a terminal still got color")
	}

	t.Setenv("NO_COLOR", "")

	os.Stderr = piped
	if !wantNoColor() {
		t.Error("stderr redirected to a file got color")
	}
}
