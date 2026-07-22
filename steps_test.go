package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunFlagParsing(t *testing.T) {
	t.Parallel()

	const pipeline = `
jobs:
- name: build
  plan:
  - task: build
    inputs: []
    run: echo hi
- name: test
  plan:
  - task: test
    inputs: []
    run: echo hi
`

	// writePipeline gives each subtest its own temp dir (and therefore its
	// own .steps/state.db, colocated with the YAML) so the parallel subtests
	// never share a state database across independent Store connections.
	writePipeline := func(t *testing.T) string {
		t.Helper()

		path := filepath.Join(t.TempDir(), "pipeline.yml")

		err := os.WriteFile(path, []byte(pipeline), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		return path
	}

	t.Run("job flag before positional path", func(t *testing.T) {
		t.Parallel()

		err := run([]string{"--job", "build", writePipeline(t)})
		if err != nil {
			t.Errorf("run: %v", err)
		}
	})

	t.Run("job flag after positional path", func(t *testing.T) {
		t.Parallel()

		err := run([]string{writePipeline(t), "--job", "test"})
		if err != nil {
			t.Errorf("run: %v", err)
		}
	})

	t.Run("job flag attached value", func(t *testing.T) {
		t.Parallel()

		err := run([]string{"--job=build", writePipeline(t)})
		if err != nil {
			t.Errorf("run: %v", err)
		}
	})

	t.Run("missing job on ambiguous pipeline", func(t *testing.T) {
		t.Parallel()

		err := run([]string{writePipeline(t)})
		if err == nil {
			t.Error("expected error when --job is omitted and multiple jobs exist")
		}
	})

	t.Run("missing pipeline path", func(t *testing.T) {
		t.Parallel()

		err := run(nil)
		if err == nil {
			t.Error("expected error for missing positional pipeline path")
		}
	})

	// TestLogLevelFlag guards the fix for debug logging being unconditionally
	// on: initLogging previously hardcoded slog.LevelDebug with no override,
	// so shell.go/docker.go's full command/output logging ran on every
	// ordinary invocation. --log-level (and STEPS_LOG_LEVEL) is now a real,
	// kong-validated flag — this only checks that a valid value parses and an
	// invalid one is rejected; the level-to-slog.Level mapping itself is
	// covered by TestParseLogLevel (main_test.go), not here, since every
	// run() call in this file's other subtests also installs a new global
	// slog default logger — asserting on slog.Default() here would race
	// against whichever other subtest's run() call happens to finish last.
	t.Run("log-level flag is accepted", func(t *testing.T) {
		t.Parallel()

		err := run([]string{"--log-level", "debug", "--job", "build", writePipeline(t)})
		if err != nil {
			t.Errorf("run: %v", err)
		}
	})

	t.Run("invalid log-level is rejected", func(t *testing.T) {
		t.Parallel()

		err := run([]string{"--log-level", "verbose", "--job", "build", writePipeline(t)})
		if err == nil {
			t.Error("expected an error for an unrecognized --log-level value")
		}
	})
}
