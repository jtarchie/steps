package cli

import (
	"log/slog"
	"os"

	"github.com/lmittmann/tint"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/telemetry"
)

// DefaultLogLevel is what InitLogging runs at before the CLI's own
// --log-level/STEPS_LOG_LEVEL has been parsed (during kong construction
// itself, and as CLI.LogLevel's own default) — never debug, so a parse
// failure (or any other pre-parse code path) can't fall back to printing
// every subsequent command/output dump by accident.
const DefaultLogLevel = "info"

// parseLogLevel maps a --log-level/STEPS_LOG_LEVEL string to a slog.Level,
// falling back to slog.LevelInfo for anything unrecognized — reachable only
// if called before kong's own enum: validation on CLI.LogLevel runs (e.g.
// DefaultLogLevel's own value, or a hypothetical future caller), never from
// a successfully parsed CLI. A standalone function (rather than inlined
// into InitLogging) so it can be unit-tested without touching the global
// slog default logger, which every run() call in this package's test suite
// also mutates — asserting on slog.Default() itself would race against
// whichever other test's run() call happens to finish last.
func parseLogLevel(level string) slog.Level {
	parsed, ok := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}[level]
	if !ok {
		return slog.LevelInfo
	}

	return parsed
}

// InitLogging installs a slog handler on stderr as the default logger,
// separate from this tool's plain stdout progress lines ("get: prs
// (version: ...)", "task: review"), at the given level (see parseLogLevel).
// Debug is what previously ran unconditionally on every invocation: shell.go/
// docker.go log the full rendered command and complete captured stdout/
// stderr at that level, so any resource check/in/out command whose
// templated source: or output embeds a credential was written to stderr on
// every ordinary run, with no way to suppress it. Defaulting to info (see
// CLI.LogLevel) makes that opt-in, via --log-level debug or
// STEPS_LOG_LEVEL=debug, rather than the permanent default.
func InitLogging(level string) {
	slog.SetDefault(slog.New(telemetry.LogHandler(events.LogHandler(tint.NewTextHandler(os.Stderr, &tint.Options{
		Level:     parseLogLevel(level),
		AddSource: true,
		NoColor:   wantNoColor(),
	})))))
}

// wantNoColor reports whether log output should skip ANSI color: either
// stderr isn't a terminal (piped, redirected to a file, captured by a
// screen reader or log tool) or the operator opted out via NO_COLOR
// (https://no-color.org). Terminal detection is a stdlib character-device
// check (no isatty dependency needed) rather than an internal/shell-style
// pattern, since this is the one place in the process that cares whether
// its own stderr is a live terminal. Checked at every InitLogging call
// rather than cached, since tests re-invoke it against different stderr
// targets.
func wantNoColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return true
	}

	info, err := os.Stderr.Stat()
	if err != nil {
		return true
	}

	return info.Mode()&os.ModeCharDevice == 0
}
