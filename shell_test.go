package main

import (
	"context"
	"testing"
	"time"
)

func TestRunShellCaptureFull(t *testing.T) {
	t.Parallel()

	t.Run("captures stdout, stderr, and exit code separately", func(t *testing.T) {
		t.Parallel()

		stdout, stderr, exitCode, err := RunShellCaptureFull(context.Background(), "echo out; echo err >&2; exit 3", t.TempDir())
		if err != nil {
			t.Fatalf("RunShellCaptureFull: %v", err)
		}

		if stdout != "out\n" {
			t.Errorf("stdout = %q, want %q", stdout, "out\n")
		}

		if stderr != "err\n" {
			t.Errorf("stderr = %q, want %q", stderr, "err\n")
		}

		if exitCode != 3 {
			t.Errorf("exitCode = %d, want 3", exitCode)
		}
	})

	t.Run("successful command has exit code 0 and a nil error", func(t *testing.T) {
		t.Parallel()

		_, _, exitCode, err := RunShellCaptureFull(context.Background(), "true", t.TempDir())
		if err != nil {
			t.Fatalf("RunShellCaptureFull: %v", err)
		}

		if exitCode != 0 {
			t.Errorf("exitCode = %d, want 0", exitCode)
		}
	})

	t.Run("a nonzero exit is not a Go error", func(t *testing.T) {
		t.Parallel()

		_, _, exitCode, err := RunShellCaptureFull(context.Background(), "exit 1", t.TempDir())
		if err != nil {
			t.Fatalf("RunShellCaptureFull returned a Go error for a normal nonzero exit: %v", err)
		}

		if exitCode != 1 {
			t.Errorf("exitCode = %d, want 1", exitCode)
		}
	})

	t.Run("a failure to start the process returns a Go error", func(t *testing.T) {
		t.Parallel()

		_, _, _, err := RunShellCaptureFull(context.Background(), "true", "/nonexistent/dir/for/sure")
		if err == nil {
			t.Error("expected an error when cwd doesn't exist")
		}
	})
}

// TestRunShellCaptureFullSignalKilled guards against exitCodeOf's -1
// sentinel being ambiguous between "never started" and "ran, then
// signal-killed" — both produce ExitCode() == -1, so a naive check
// misclassifies a context-timeout kill as a start failure and drops its
// captured output.
func TestRunShellCaptureFullSignalKilled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// exec replaces the shell with sleep (same PID), so the context's
	// SIGKILL lands directly on it instead of orphaning a grandchild that
	// would keep the output pipe open until it exits on its own — keeps
	// this test fast instead of blocking for the full sleep duration.
	stdout, _, exitCode, err := RunShellCaptureFull(ctx, "echo partial; exec sleep 5", t.TempDir())
	if err != nil {
		t.Fatalf("a signal-killed process must be reported as data, not a Go error (regression: it was misclassified as \"failed to start\"): %v", err)
	}

	if stdout != "partial\n" {
		t.Errorf("stdout = %q, want the output captured before the kill", stdout)
	}

	if exitCode != -1 {
		t.Errorf("exitCode = %d, want -1 for a signal-killed process", exitCode)
	}
}

func TestRunShellCapture(t *testing.T) {
	t.Parallel()

	t.Run("returns captured stdout on success", func(t *testing.T) {
		t.Parallel()

		out, err := RunShellCapture(context.Background(), "echo hello", t.TempDir())
		if err != nil {
			t.Fatalf("RunShellCapture: %v", err)
		}

		if string(out) != "hello\n" {
			t.Errorf("out = %q, want %q", out, "hello\n")
		}
	})

	t.Run("a nonzero exit is a Go error, unlike RunShellCaptureFull", func(t *testing.T) {
		t.Parallel()

		out, err := RunShellCapture(context.Background(), "echo partial; exit 1", t.TempDir())
		if err == nil {
			t.Fatal("expected an error for a nonzero exit")
		}

		if out != nil {
			t.Errorf("out = %q, want nil on error", out)
		}
	})
}
