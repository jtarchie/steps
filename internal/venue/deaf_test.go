package venue

// A worker that stops answering mid-command.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/wire"
)

// deafExecEnv makes the helper-process shim greet, take the tree, and then go
// silent the moment it is asked to run something — answering neither the
// command nor the cancel.
const deafExecEnv = "STEPS_TEST_DEAF_EXEC"

// serveDeafShim is a worker that wedged. It is not malicious and not crashed:
// the connection stays open and the process stays alive, exactly as a stalled
// disk, a hung child or a silently dead network path leaves it. Nothing this
// end asks for ever comes back.
func serveDeafShim() {
	decoder := wire.NewDecoder(os.Stdin)
	encoder := wire.NewEncoder(os.Stdout)

	for {
		frame, err := decoder.Read()
		if err != nil {
			return
		}

		switch frame.Type {
		case wire.FrameHello:
			_ = encoder.WriteJSON(wire.FrameHelloOK, frame.Op, wire.HelloOK{
				Protocol: wire.Protocol,
				Workdir:  os.TempDir(),
			})
		case wire.FrameEnd:
			_ = encoder.Write(wire.Frame{Type: wire.FrameEnd, Op: frame.Op})
		case wire.FrameExec:
			// The whole point: no stdout, no exit, and no answer to the cancel
			// that follows. Keep reading so the connection stays up.
		case wire.FrameHelloOK, wire.FrameUpload, wire.FrameStdout, wire.FrameStderr,
			wire.FrameExit, wire.FrameFetch, wire.FrameData, wire.FrameCancel,
			wire.FrameError, wire.FrameBye:
		}
	}
}

// TestVenueCancellationEndsACommandAWedgedWorkerIgnores is what makes timeout:
// mean something on a worker.
//
// A cancel is a frame, not a signal — so it only stops a shim that is still
// listening. Against one that is not, this end used to sit in a synchronous
// ReadFull with no deadline: the cancel went out, nothing came back, and the
// step waited for as long as the machine stayed up. timeout:, fail_fast, race:
// and Ctrl-C each ended nothing.
func TestVenueCancellationEndsACommandAWedgedWorkerIgnores(t *testing.T) {
	t.Setenv(deafExecEnv, "1")

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	ctx, cancel := context.WithTimeout(context.Background(), shortWait)
	defer cancel()

	returned := make(chan error, 1)

	go func() { returned <- runner.Run(ctx, "anything") }()

	// Generously past the grace period the connection is torn down after, and
	// still nowhere near "forever".
	limit := time.NewTimer(30 * time.Second)
	defer limit.Stop()

	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("a command a worker never answered reported success")
		}

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error = %v, want it to carry the deadline — an interrupted step is not a failed one", err)
		}
	case <-limit.C:
		t.Fatal("the command never returned: the cancel frame went unanswered and the read has no deadline, so the step waits on the worker forever")
	}
}
