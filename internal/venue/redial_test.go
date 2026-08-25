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
