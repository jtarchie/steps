package venue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A worker that prints without end must not grow this process's memory with it: what a readiness wait keeps is bounded, and it is the tail, where the markers are.
func TestSessionOutputKeepsABoundedTail(t *testing.T) {
	t.Parallel()

	client := newTestSSHD(t).Dial(t)

	out, err := sessionOutput(context.Background(), client, "head -c 20000000 /dev/zero; echo ready", true)
	if err != nil {
		t.Fatalf("sessionOutput: %v", err)
	}

	if len(out) != maxSessionOutput {
		t.Errorf("kept %d bytes of a 20MB answer, want exactly %d", len(out), maxSessionOutput)
	}

	if !strings.HasSuffix(out, "ready\n") {
		t.Errorf("the kept output does not end with the marker the wait looks for: %q", out[max(0, len(out)-20):])
	}
}

// ssh exec takes no context: a job that stops wanting the wait must end it, not sit out a command that never returns.
func TestSessionOutputEndsWhenTheContextDoes(t *testing.T) {
	t.Parallel()

	client := newTestSSHD(t).Dial(t)

	dir := t.TempDir()
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")

	for _, fifo := range []string{started, release} {
		err := syscall.Mkfifo(fifo, 0o600)
		if err != nil {
			t.Fatalf("mkfifo: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		_, err := sessionOutput(ctx, client, "echo up > "+shellQuote(started)+"; cat "+shellQuote(release), false)
		done <- err
	}()

	// Blocks until the remote command is running, so the cancel lands mid-wait.
	_, err := os.ReadFile(started) //nolint:gosec // the test's own fifo in its own temp dir
	if err != nil {
		t.Fatalf("reading the start signal: %v", err)
	}

	// The test sshd does not kill a command its client left, so the blocked cat is let go here or its goroutine outlives the test.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })

	cancel()

	err = <-done
	if !errors.Is(err, context.Canceled) {
		t.Errorf("sessionOutput after cancel = %v, want the cancellation", err)
	}
}
