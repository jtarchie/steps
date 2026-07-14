package main

import (
	"context"
	"testing"
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
