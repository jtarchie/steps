package shell

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
)

func TestPrefixedWriter(t *testing.T) {
	t.Parallel()

	t.Run("single write ending in newline is prefixed immediately", func(t *testing.T) {
		t.Parallel()

		var dst bytes.Buffer

		pw := newPrefixedWriter("[x] ", &dst)

		n, err := pw.Write([]byte("hello\n"))
		if err != nil || n != len("hello\n") {
			t.Fatalf("Write: n=%d err=%v", n, err)
		}

		if got := dst.String(); got != "[x] hello\n" {
			t.Errorf("dst = %q, want %q", got, "[x] hello\n")
		}
	})

	t.Run("multiple lines in one write are each prefixed", func(t *testing.T) {
		t.Parallel()

		var dst bytes.Buffer

		pw := newPrefixedWriter("[x] ", &dst)

		_, _ = pw.Write([]byte("one\ntwo\nthree\n"))

		want := "[x] one\n[x] two\n[x] three\n"
		if got := dst.String(); got != want {
			t.Errorf("dst = %q, want %q", got, want)
		}
	})

	t.Run("a line split across writes streams each piece immediately, one prefix", func(t *testing.T) {
		t.Parallel()

		var dst bytes.Buffer

		pw := newPrefixedWriter("[x] ", &dst)

		// The whole point of the streaming rewrite: a partial (newline-less)
		// write must appear at dst NOW, not be held until a newline arrives.
		// This is what made an agent's token-by-token output actually stream.
		_, _ = pw.Write([]byte("hel"))

		if got := dst.String(); got != "[x] hel" {
			t.Errorf("dst = %q after partial write, want %q (must stream, not buffer)", got, "[x] hel")
		}

		_, _ = pw.Write([]byte("lo\n"))

		// The continuation gets no second prefix — it's the same line.
		if got := dst.String(); got != "[x] hello\n" {
			t.Errorf("dst = %q, want %q", got, "[x] hello\n")
		}
	})

	t.Run("a chunk with an interior newline splits into two prefixed lines", func(t *testing.T) {
		t.Parallel()

		var dst bytes.Buffer

		pw := newPrefixedWriter("[x] ", &dst)

		// A single delta straddling a line break (common in streamed model
		// output) must prefix the line that starts after the newline.
		_, _ = pw.Write([]byte("end of one\nstart of two"))

		if got := dst.String(); got != "[x] end of one\n[x] start of two" {
			t.Errorf("dst = %q, want %q", got, "[x] end of one\n[x] start of two")
		}
	})
}

func TestPrefixedWriterFlush(t *testing.T) {
	t.Parallel()

	t.Run("flush terminates a streamed partial line with a trailing newline", func(t *testing.T) {
		t.Parallel()

		var dst bytes.Buffer

		pw := newPrefixedWriter("[x] ", &dst)

		// The content is already streamed on the partial write; flush only
		// adds the missing newline so the next output starts on its own line.
		_, _ = pw.Write([]byte("no newline at all"))

		if got := dst.String(); got != "[x] no newline at all" {
			t.Errorf("dst = %q before flush, want the streamed partial %q", got, "[x] no newline at all")
		}

		pw.flush()

		if got := dst.String(); got != "[x] no newline at all\n" {
			t.Errorf("dst = %q, want %q", got, "[x] no newline at all\n")
		}
	})

	t.Run("flush is a no-op after a newline-terminated line", func(t *testing.T) {
		t.Parallel()

		var dst bytes.Buffer

		pw := newPrefixedWriter("[x] ", &dst)

		_, _ = pw.Write([]byte("complete\n"))
		pw.flush()

		if got := dst.String(); got != "[x] complete\n" {
			t.Errorf("dst = %q, want %q (flush after a complete line must not duplicate/alter it)", got, "[x] complete\n")
		}
	})
}

func TestPrefixedStream(t *testing.T) {
	t.Parallel()

	t.Run("empty label returns dst unchanged, unprefixed", func(t *testing.T) {
		t.Parallel()

		var dst bytes.Buffer

		w, flush := prefixedStream("", &dst)

		_, _ = w.Write([]byte("plain\n"))
		flush()

		if got := dst.String(); got != "plain\n" {
			t.Errorf("dst = %q, want %q (unprefixed)", got, "plain\n")
		}
	})

	t.Run("non-empty label wraps dst in brackets", func(t *testing.T) {
		t.Parallel()

		var dst bytes.Buffer

		w, flush := prefixedStream("worker", &dst)

		_, _ = w.Write([]byte("line\n"))
		flush()

		if got := dst.String(); got != "[worker] line\n" {
			t.Errorf("dst = %q, want %q", got, "[worker] line\n")
		}
	})
}

func TestPrefixLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		label string
		s     string
		want  string
	}{
		{"empty label is a no-op", "", "hello\nworld\n", "hello\nworld\n"},
		{"empty string is a no-op", "x", "", ""},
		{"single line, no trailing newline", "x", "hello", "[x] hello"},
		{"multi-line with trailing newline", "x", "one\ntwo\n", "[x] one\n[x] two\n"},
		{"multi-line without trailing newline", "x", "one\ntwo", "[x] one\n[x] two"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := PrefixLines(tt.label, tt.s); got != tt.want {
				t.Errorf("PrefixLines(%q, %q) = %q, want %q", tt.label, tt.s, got, tt.want)
			}
		})
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe, returning
// everything fn wrote. Not safe alongside other tests running in parallel
// that also touch os.Stdout — callers must not use t.Parallel(). Duplicated
// from internal/agent/step_test.go rather than exported cross-package,
// matching this repo's convention of small duplicated test helpers over new
// cross-package edges.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	orig := os.Stdout
	os.Stdout = w

	fn()

	_ = w.Close()

	os.Stdout = orig

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}

	return string(data)
}

// TestHostRunnerWithLabelPrefixesLiveOutput is an end-to-end check (not just
// prefixedWriter's unit tests) that WithLabel actually reaches Run's live
// os.Stdout stream — the real wiring a run_shell/task run: caller depends
// on, not just the writer's own logic in isolation.
func TestHostRunnerWithLabelPrefixesLiveOutput(t *testing.T) {
	got := captureStdout(t, func() {
		runner := HostRunner{}.WithLabel("build")

		err := runner.Run(context.Background(), "echo one; echo two")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	})

	want := "[build] one\n[build] two\n"
	if got != want {
		t.Errorf("captured stdout = %q, want %q", got, want)
	}
}

// TestHostRunnerRunCaptureFullLimitedStreamedPrefixesAndCaptures verifies
// the new streamed capture path (used by run_shell/custom tools) both
// prints live, prefixed output AND still returns the captured result data —
// the two things RunCaptureFullLimited already did (capture) and Run
// already did (stream) were never previously combined in one call.
func TestHostRunnerRunCaptureFullLimitedStreamedPrefixesAndCaptures(t *testing.T) {
	runner := HostRunner{}.WithLabel("agent")

	var stdout, stderr string

	var exitCode int

	got := captureStdout(t, func() {
		var err error

		stdout, stderr, exitCode, err = runner.RunCaptureFullLimitedStreamed(context.Background(), "echo hi", 0, "")
		if err != nil {
			t.Fatalf("RunCaptureFullLimitedStreamed: %v", err)
		}
	})

	if want := "[agent] hi\n"; got != want {
		t.Errorf("captured stdout stream = %q, want %q", got, want)
	}

	if stdout != "hi\n" {
		t.Errorf("returned stdout = %q, want %q (unprefixed — this is the model-facing data)", stdout, "hi\n")
	}

	if stderr != "" {
		t.Errorf("returned stderr = %q, want empty", stderr)
	}

	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
}

// TestHostRunnerRunCaptureFullLimitedStaysSilent confirms the pre-existing,
// non-streamed capture methods are unaffected by WithLabel — a when: guard
// or an assert-mode task's own command must stay invisible until printed
// explicitly (see printTaskOutput), not suddenly start streaming just
// because a label was set for other purposes on the same runner.
func TestHostRunnerRunCaptureFullLimitedStaysSilent(t *testing.T) {
	runner := HostRunner{}.WithLabel("guard")

	got := captureStdout(t, func() {
		_, _, _, err := runner.RunCaptureFullLimited(context.Background(), "echo hi", 0, "")
		if err != nil {
			t.Fatalf("RunCaptureFullLimited: %v", err)
		}
	})

	if got != "" {
		t.Errorf("captured stdout = %q, want empty (RunCaptureFullLimited must never stream)", got)
	}
}
