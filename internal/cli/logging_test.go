package cli

import (
	"log/slog"
	"testing"
)

// TestParseLogLevel covers the level-to-slog.Level mapping InitLogging uses,
// including the fallback for an unrecognized value (reachable only before
// kong's own enum: validation on CLI.LogLevel runs). Deliberately does not
// touch slog.Default(): every Run() call elsewhere in this package's test
// suite also installs a new global default logger, so asserting on shared
// global state here would race against whichever other test's Run() call
// happens to finish last.
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
