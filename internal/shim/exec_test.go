package shim

// Cancelling one command while another is running.

import (
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// TestCancelReachesTheFirstOfTwoRunningCommands is the half of the
// registration bug the op check alone did not close.
//
// The frame loop deliberately keeps reading while a command runs, so a second
// exec can register before the first finishes — endCommand's own comment says
// so. With one registration slot, that second exec silently deregistered the
// first, and a cancel aimed at the first then found nothing and was dropped:
// timeout:, fail_fast and race: ended nothing while the step ran to completion
// on the worker.
func TestCancelReachesTheFirstOfTwoRunningCommands(t *testing.T) {
	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})
	peer.hello()

	first := peer.next()
	peer.send(wire.FrameExec, first, wire.Exec{Command: "sleep 30"})

	// A second command registers while the first is still running, which is
	// what overwrote the first's registration.
	second := peer.next()
	peer.send(wire.FrameExec, second, wire.Exec{Command: "sleep 30"})

	peer.sendEmpty(wire.FrameCancel, first)

	for {
		frame := peer.read()
		if frame.Type != wire.FrameExit {
			continue
		}

		if frame.Op == second {
			t.Fatal("the cancel killed the second command — it was aimed at the first")
		}

		if frame.Op == first {
			break
		}
	}

	// The second is still running, and this session cannot end while it is.
	peer.sendEmpty(wire.FrameCancel, second)

	for {
		frame := peer.read()
		if frame.Type == wire.FrameExit && frame.Op == second {
			return
		}
	}
}

// TestASecondExecOnTheSameOperationIsRefused is dockerStreams.add's rule on the
// command registry, which is the other map this session keys by a wire value.
//
// Overwriting made the FIRST command's endCommand delete the SECOND's
// registration, so a FrameCancel aimed at the running command found nothing —
// and timeout:, fail_fast and race: ended nothing while it ran to completion on
// the worker.
func TestASecondExecOnTheSameOperationIsRefused(t *testing.T) {
	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})
	peer.hello()

	op := peer.next()
	peer.send(wire.FrameExec, op, wire.Exec{Command: "echo running; sleep 30"})

	// Sent only once the first command owns the op, which is the state the
	// duplicate has to be refused against.
	frame := peer.read()
	if frame.Type != wire.FrameStdout {
		t.Fatalf("frame type = %v, want the first command to announce itself", frame.Type)
	}

	peer.send(wire.FrameExec, op, wire.Exec{Command: "echo second"})

	frame = peer.readAny()
	if frame.Type != wire.FrameError {
		t.Fatalf("frame type = %v for a second exec on operation %d, want a refusal", frame.Type, op)
	}

	// The first command still owns the registration, so the cancel that ends
	// this session reaches it.
	peer.sendEmpty(wire.FrameCancel, op)

	for {
		next := peer.read()
		if next.Type == wire.FrameExit && next.Op == op {
			return
		}
	}
}
