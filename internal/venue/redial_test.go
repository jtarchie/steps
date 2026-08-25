package venue

// What happens when the worker dies mid-conversation.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

// TestVenueRedialsAfterTheWorkerDies pins the difference between a host that
// could never be reached and a conversation that died after it opened. The
// first sticks — retrying an unreachable host once per command buys nothing
// but timeouts. The second must not: the step's next command is entitled to a
// fresh dial, or attempts: on a placed step retries into a dead pipe and every
// retry fails for a reason the author cannot see.
func TestVenueRedialsAfterTheWorkerDies(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, localWorker(t, cwd, "out"))

	err := runner.Run(context.Background(), "echo one > out/first.txt")
	if err != nil {
		t.Fatalf("the first command failed: %v", err)
	}

	err = runner.Run(context.Background(), "kill -9 $PPID")
	if err == nil {
		t.Fatal("killing the shim mid-command reported success")
	}

	// Infrastructure, not a verdict: a worker that vanished must never read
	// as the command having run and failed.
	if shell.IsExitError(err) {
		t.Fatalf("a dead worker was reported as a command's exit: %v", err)
	}

	// The next command reaches a fresh shim, and the tree it sees is the
	// LOCAL one — which already holds what the dead worker sent back after
	// the first command, so earlier progress survives the redial.
	err = runner.Run(context.Background(), "cat out/first.txt > out/second.txt")
	if err != nil {
		t.Fatalf("the command after the worker died did not reach a fresh shim: %v", err)
	}

	got := mustRead(t, filepath.Join(cwd, "out", "second.txt"))
	if got != "one\n" {
		t.Errorf("out/second.txt = %q, want %q — the redialled shim did not see the tree the first command produced", got, "one\n")
	}
}

// TestVenueKeepsTheWorkerAfterAShimError pins the boundary the redial guards:
// an error frame is the shim answering over a healthy transport — an operation
// that failed, not a worker that died. Redialling on it would abandon the
// worker's scratch and re-ship the whole tree for an error a fresh dial cannot
// fix.
func TestVenueKeepsTheWorkerAfterAShimError(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, localWorker(t, cwd, "out"))

	// A fifo in a declared output makes the shim's fetch refuse: an error
	// frame, with the conversation still open.
	err := runner.Run(context.Background(), "echo kept > scratch.txt; mkfifo out/fifo")
	if err == nil {
		t.Fatal("a fifo in an output was fetched")
	}

	// The next command reaches the SAME shim: its scratch — which the local
	// tree does not hold — is still there, exactly as it is between any two
	// commands of a step.
	err = runner.Run(context.Background(), "rm out/fifo; cat scratch.txt > out/second.txt")
	if err != nil {
		t.Fatalf("the command after a shim error failed: %v", err)
	}

	if got := mustRead(t, filepath.Join(cwd, "out", "second.txt")); got != "kept\n" {
		t.Errorf("out/second.txt = %q, want %q — the worker was redialled and its scratch lost", got, "kept\n")
	}
}
