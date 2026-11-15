package shell

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/redact"
)

// A value straddling the capture limit was cut before it was masked, so the head of it survived; masking in front of the capture means no fragment does.
func TestCaptureMasksAValueCutAtTheLimit(t *testing.T) {
	t.Parallel()

	const secret = "straddling-secret-value-0123456789"

	secrets := &redact.Secrets{}
	secrets.Add(secret)

	runner, err := NewRunner(RunnerSpec{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	ctx := redact.WithSecrets(t.Context(), secrets)

	// 90 bytes of filler then the value: a 100-byte limit cuts it ten bytes in.
	stdout, _, _, err := RunCaptureFullLimitedStreamed(ctx, runner, `printf '%090d' 0; printf '%s and more' '`+secret+`'`, 100, "")
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(stdout, secret[:10]) {
		t.Errorf("the truncated capture holds a fragment of the secret: %q", stdout)
	}
}

// A run's values grow while it runs (an in_parallel: sibling resolving a ((var))), so a stream that started with nothing to mask still masks a value registered after it started.
func TestStreamMasksAValueAddedAfterItStarted(t *testing.T) {
	t.Parallel()

	const secret = "late-registered-secret-value"

	dir := t.TempDir()

	runner, err := NewRunner(RunnerSpec{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	secrets := &redact.Secrets{}
	live := &signalBuffer{started: make(chan struct{})}
	ctx := redact.WithSecrets(events.WithOutput(t.Context(), events.Output{Stdout: live, Stderr: io.Discard}), secrets)

	type outcome struct {
		stdout string
		err    error
	}

	done := make(chan outcome, 1)

	go func() {
		stdout, _, _, err := RunCaptureFullLimitedStreamed(ctx, runner.WithLabel("tool"), `printf 'started\n'; while [ ! -f go ]; do sleep 0.01; done; printf '%s\n' '`+secret+`'`, 0, "")
		done <- outcome{stdout, err}
	}()

	<-live.started
	secrets.Add(secret)

	err = os.WriteFile(filepath.Join(dir, "go"), nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}

	if strings.Contains(live.String(), secret) {
		t.Errorf("the live stream showed a value registered after it started: %q", live.String())
	}

	if strings.Contains(got.stdout, secret) {
		t.Errorf("the capture holds a value registered after it started: %q", got.stdout)
	}
}

type signalBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	started chan struct{}
	once    sync.Once
}

func (b *signalBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.once.Do(func() { close(b.started) })

	return b.buf.Write(p) //nolint:wrapcheck // bytes.Buffer never errors
}

func (b *signalBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// A labeled live stream holds its partial line until Flush; a tail the scrubber held back has to reach it before that, or the line is broken in two and its end left dangling after the command's result.
func TestLiveStreamShowsATailTheScrubberHeldBack(t *testing.T) {
	t.Parallel()

	secrets := &redact.Secrets{}
	secrets.Add("s3cr3t-value-for-flush")

	runner, err := NewRunner(RunnerSpec{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	var live bytes.Buffer

	ctx := redact.WithSecrets(events.WithOutput(t.Context(), events.Output{Stdout: &live, Stderr: io.Discard}), secrets)

	_, _, _, err = RunCaptureFullLimitedStreamed(ctx, runner.WithLabel("tool"), `printf 'tail s3c'`, 0, "")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(live.String(), "tail s3c") {
		t.Errorf("the terminal lost the held-back tail: %q", live.String())
	}
}
