package shell

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestNewCaptureWriter(t *testing.T) {
	t.Parallel()

	t.Run("maxBytes <= 0 returns an unbounded writer", func(t *testing.T) {
		t.Parallel()

		for _, maxBytes := range []int{0, -1} {
			w := newCaptureWriter(maxBytes)
			if _, ok := w.(*unboundedWriter); !ok {
				t.Errorf("newCaptureWriter(%d) = %T, want *unboundedWriter", maxBytes, w)
			}
		}
	})

	t.Run("maxBytes > 0 returns a bounded writer", func(t *testing.T) {
		t.Parallel()

		w := newCaptureWriter(10)
		if _, ok := w.(*boundedWriter); !ok {
			t.Errorf("newCaptureWriter(10) = %T, want *boundedWriter", w)
		}
	})
}

func TestUnboundedWriter(t *testing.T) {
	t.Parallel()

	w := &unboundedWriter{}

	n, err := w.Write([]byte("hello "))
	if err != nil || n != 6 {
		t.Fatalf("Write = (%d, %v), want (6, nil)", n, err)
	}

	_, _ = w.Write([]byte("world"))

	if got := w.result(); got != "hello world" {
		t.Errorf("result() = %q, want %q", got, "hello world")
	}
}

func TestBoundedWriter(t *testing.T) {
	t.Parallel()

	t.Run("under the cap is untruncated", func(t *testing.T) {
		t.Parallel()

		w := &boundedWriter{max: 100}

		n, err := w.Write([]byte("hello"))
		if err != nil || n != 5 {
			t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
		}

		if got := w.result(); got != "hello" {
			t.Errorf("result() = %q, want %q", got, "hello")
		}
	})

	t.Run("over the cap is truncated with a marker, but Write never short-writes", func(t *testing.T) {
		t.Parallel()

		w := &boundedWriter{max: 5}

		n, err := w.Write([]byte("hello world"))
		if err != nil {
			t.Fatalf("Write returned an error: %v", err)
		}

		if n != len("hello world") {
			t.Errorf("Write returned n=%d, want %d (io.Writer must never short-write)", n, len("hello world"))
		}

		want := "hello\n... [truncated 6 bytes]"
		if got := w.result(); got != want {
			t.Errorf("result() = %q, want %q", got, want)
		}
	})

	t.Run("cap spans multiple writes", func(t *testing.T) {
		t.Parallel()

		w := &boundedWriter{max: 8}

		_, _ = w.Write([]byte("1234"))
		_, _ = w.Write([]byte("5678"))
		_, _ = w.Write([]byte("9999"))

		want := "12345678\n... [truncated 4 bytes]"
		if got := w.result(); got != want {
			t.Errorf("result() = %q, want %q", got, want)
		}
	})

	t.Run("exactly at the cap has no marker", func(t *testing.T) {
		t.Parallel()

		w := &boundedWriter{max: 5}

		_, _ = w.Write([]byte("hello"))

		if got := w.result(); got != "hello" {
			t.Errorf("result() = %q, want %q (no truncation marker when exactly at the cap)", got, "hello")
		}
	})
}

// assertCapped fails the test unless got is exactly wantPrefix followed by a
// truncation marker reporting wantOverflow bytes dropped.
func assertCapped(t *testing.T, name, got, wantPrefix string, wantOverflow int) {
	t.Helper()

	want := wantPrefix + fmt.Sprintf("\n... [truncated %d bytes]", wantOverflow)
	if got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

func TestHostRunnerRunCaptureFullLimited(t *testing.T) {
	t.Parallel()

	t.Run("caps stdout and stderr independently while the command runs", func(t *testing.T) {
		t.Parallel()

		runner := HostRunner{cwd: t.TempDir()}

		stdout, stderr, exitCode, err := runner.RunCaptureFullLimited(
			context.Background(), "printf '%0.sA' $(seq 1 50); printf '%0.sB' $(seq 1 50) 1>&2", 10,
		)
		if err != nil {
			t.Fatalf("RunCaptureFullLimited: %v", err)
		}

		if exitCode != 0 {
			t.Errorf("exitCode = %d, want 0", exitCode)
		}

		assertCapped(t, "stdout", stdout, strings.Repeat("A", 10), 40)
		assertCapped(t, "stderr", stderr, strings.Repeat("B", 10), 40)
	})

	t.Run("output under the cap is untouched", func(t *testing.T) {
		t.Parallel()

		runner := HostRunner{cwd: t.TempDir()}

		stdout, _, _, err := runner.RunCaptureFullLimited(context.Background(), "echo short", 100)
		if err != nil {
			t.Fatalf("RunCaptureFullLimited: %v", err)
		}

		if stdout != "short\n" {
			t.Errorf("stdout = %q, want %q", stdout, "short\n")
		}
	})

	t.Run("a normal nonzero exit is still data, not a Go error", func(t *testing.T) {
		t.Parallel()

		runner := HostRunner{cwd: t.TempDir()}

		_, _, exitCode, err := runner.RunCaptureFullLimited(context.Background(), "exit 7", 100)
		if err != nil {
			t.Fatalf("RunCaptureFullLimited returned a Go error for a normal nonzero exit: %v", err)
		}

		if exitCode != 7 {
			t.Errorf("exitCode = %d, want 7", exitCode)
		}
	})
}
