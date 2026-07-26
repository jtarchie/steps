package shell

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewCaptureWriter(t *testing.T) {
	t.Parallel()

	t.Run("maxBytes <= 0 returns an unbounded writer, regardless of spillDir", func(t *testing.T) {
		t.Parallel()

		for _, maxBytes := range []int{0, -1} {
			w := newCaptureWriter(maxBytes, "")
			if _, ok := w.(*unboundedWriter); !ok {
				t.Errorf("newCaptureWriter(%d, \"\") = %T, want *unboundedWriter", maxBytes, w)
			}

			w = newCaptureWriter(maxBytes, t.TempDir())
			if _, ok := w.(*unboundedWriter); !ok {
				t.Errorf("newCaptureWriter(%d, dir) = %T, want *unboundedWriter", maxBytes, w)
			}
		}
	})

	t.Run("maxBytes > 0 and no spillDir returns a bounded writer", func(t *testing.T) {
		t.Parallel()

		w := newCaptureWriter(10, "")
		if _, ok := w.(*boundedWriter); !ok {
			t.Errorf("newCaptureWriter(10, \"\") = %T, want *boundedWriter", w)
		}
	})

	t.Run("maxBytes > 0 and a spillDir returns a spill writer", func(t *testing.T) {
		t.Parallel()

		w := newCaptureWriter(10, t.TempDir())
		if _, ok := w.(*spillWriter); !ok {
			t.Errorf("newCaptureWriter(10, dir) = %T, want *spillWriter", w)
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

// readOneSpillFile reads the single spill file expected under dir, failing
// the test if there isn't exactly one.
func readOneSpillFile(t *testing.T, dir string) string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir(%q) = (%v entries, %v), want exactly one spill file", dir, entries, err)
	}

	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name())) //nolint:gosec // test-owned path under t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	return string(data)
}

func TestSpillWriterUnderCap(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := &spillWriter{max: 100, dir: dir}

	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}

	if got := w.result(); got != "hello" {
		t.Errorf("result() = %q, want %q", got, "hello")
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected no spill file for under-cap output, found %d entries", len(entries))
	}
}

func TestSpillWriterOverCap(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := &spillWriter{max: 5, dir: dir}

	full := "hello world"

	n, err := w.Write([]byte(full))
	if err != nil {
		t.Fatalf("Write returned an error: %v", err)
	}

	if n != len(full) {
		t.Errorf("Write returned n=%d, want %d (io.Writer must never short-write)", n, len(full))
	}

	got := w.result()

	if !strings.Contains(got, "<persistent_file>") {
		t.Errorf("result() = %q, want it to contain the <persistent_file> pointer tag", got)
	}

	if !strings.Contains(got, fmt.Sprintf("The requested content was %d bytes.", len(full))) {
		t.Errorf("result() = %q, want it to report the true total size (%d bytes)", got, len(full))
	}

	if !strings.Contains(got, "hello") {
		t.Errorf("result() = %q, want it to include a head preview containing %q", got, "hello")
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir(%q) = (%v entries, %v), want exactly one spill file", dir, entries, err)
	}

	spillPath := filepath.Join(dir, entries[0].Name())
	if !strings.Contains(got, spillPath) {
		t.Errorf("result() = %q, want it to name the spill file path %q", got, spillPath)
	}

	data := readOneSpillFile(t, dir)
	if data != full {
		t.Errorf("spill file content = %q, want the full stream %q", data, full)
	}
}

func TestSpillWriterMultipleWrites(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := &spillWriter{max: 8, dir: dir}

	_, _ = w.Write([]byte("1234"))
	_, _ = w.Write([]byte("5678"))
	_, _ = w.Write([]byte("9999"))

	got := w.result()
	if !strings.Contains(got, "<persistent_file>") {
		t.Errorf("result() = %q, want it to contain the <persistent_file> pointer tag", got)
	}

	if data := readOneSpillFile(t, dir); data != "123456789999" {
		t.Errorf("spill file content = %q, want %q", data, "123456789999")
	}
}

func TestSpillWriterExactlyAtCap(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := &spillWriter{max: 5, dir: dir}

	_, _ = w.Write([]byte("hello"))

	if got := w.result(); got != "hello" {
		t.Errorf("result() = %q, want %q (no spill when exactly at the cap)", got, "hello")
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected no spill file when exactly at the cap, found %d entries", len(entries))
	}
}

func TestSpillWriterBeyondSpillMaxBytes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := &spillWriter{max: 2, dir: dir}

	total := spillMaxBytes + 100
	chunk := strings.Repeat("x", total)

	_, _ = w.Write([]byte(chunk))

	got := w.result()
	if !strings.Contains(got, fmt.Sprintf("The requested content was %d bytes.", total)) {
		t.Errorf("result() = %q, want it to report the true total size", got)
	}

	data := readOneSpillFile(t, dir)
	if !strings.HasSuffix(data, fmt.Sprintf("\n... [truncated %d bytes]", total-spillMaxBytes)) {
		t.Errorf("spill file did not end with the expected truncation marker; last 100 bytes: %q",
			data[max(0, len(data)-100):])
	}
}

func TestFormatBytes(t *testing.T) {
	t.Parallel()

	cases := map[int]string{
		0:                    "0 B",
		999:                  "999 B",
		1024:                 "1.0 KB",
		1536:                 "1.5 KB",
		1024 * 1024:          "1.0 MB",
		1024*1024 + 512*1024: "1.5 MB",
	}

	for n, want := range cases {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestHostRunnerRunCaptureFullLimited(t *testing.T) {
	t.Parallel()

	t.Run("caps stdout and stderr independently while the command runs", func(t *testing.T) {
		t.Parallel()

		runner := HostRunner{cwd: t.TempDir()}

		stdout, stderr, exitCode, err := runner.RunCaptureFullLimited(
			context.Background(), "printf '%0.sA' $(seq 1 50); printf '%0.sB' $(seq 1 50) 1>&2", 10, "",
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

		stdout, _, _, err := runner.RunCaptureFullLimited(context.Background(), "echo short", 100, "")
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

		_, _, exitCode, err := runner.RunCaptureFullLimited(context.Background(), "exit 7", 100, "")
		if err != nil {
			t.Fatalf("RunCaptureFullLimited returned a Go error for a normal nonzero exit: %v", err)
		}

		if exitCode != 7 {
			t.Errorf("exitCode = %d, want 7", exitCode)
		}
	})
}

// TestHostRunnerRunCaptureFullLimitedSpillDir is split out from
// TestHostRunnerRunCaptureFullLimited to keep that function's cyclomatic
// complexity under the linter's cap.
func TestHostRunnerRunCaptureFullLimitedSpillDir(t *testing.T) {
	t.Parallel()

	spillDir := t.TempDir()
	runner := HostRunner{cwd: t.TempDir()}

	stdout, _, exitCode, err := runner.RunCaptureFullLimited(
		context.Background(), "printf '%0.sA' $(seq 1 50)", 10, spillDir,
	)
	if err != nil {
		t.Fatalf("RunCaptureFullLimited: %v", err)
	}

	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}

	if !strings.Contains(stdout, "<persistent_file>") {
		t.Errorf("stdout = %q, want it to contain the <persistent_file> pointer tag", stdout)
	}

	entries, err := os.ReadDir(spillDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir(%q) = (%v entries, %v), want exactly one spill file", spillDir, entries, err)
	}

	data, err := os.ReadFile(filepath.Join(spillDir, entries[0].Name())) //nolint:gosec // test-owned path under t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if string(data) != strings.Repeat("A", 50) {
		t.Errorf("spill file content = %q, want %q", string(data), strings.Repeat("A", 50))
	}
}
