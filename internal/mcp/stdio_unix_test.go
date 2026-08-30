//go:build unix

package mcp

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestCommandTransportCancelReapsGrandchildren covers what killing the direct
// child alone does not. A real stdio server is often a launcher — npx starting
// node, gopls starting `go` — so signalling only the process steps spawned
// leaves the work it started running, still holding the inherited stderr pipe.
// Under `steps web`, which reconnects on every poll, that is unbounded
// process-table growth from a pipeline that looks idle.
//
// Uses a shell that forks a long sleep and prints its pid, so the assertion is
// about a process steps never had a handle to.
func TestCommandTransportCancelReapsGrandchildren(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Fork a sleep, report its pid, then block: the direct child has to stay
	// alive until cancellation or it proves nothing.
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 300 & echo $! ; sleep 300")
	cmd.WaitDelay = stdioWaitDelay
	setProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}

	err = cmd.Start()
	if err != nil {
		t.Skipf("cannot start a forking shell here: %v", err)
	}

	var grandchild int

	_, err = fmt.Fscanln(stdout, &grandchild)
	if err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}

	if !processAlive(grandchild) {
		t.Fatalf("grandchild %d was not running before cancellation", grandchild)
	}

	cancel()

	_ = cmd.Wait()

	// The group kill is delivered and reaped asynchronously, so this waits for
	// the outcome rather than asserting on the instant after cancel.
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(grandchild) {
		if time.Now().After(deadline) {
			// Do not leave a stray sleep behind for whoever runs next.
			_ = syscall.Kill(grandchild, syscall.SIGKILL)

			t.Fatalf("grandchild %d survived cancellation of its parent", grandchild)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// processAlive reports whether pid still exists. Signal 0 runs the existence
// and permission checks without delivering anything.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
