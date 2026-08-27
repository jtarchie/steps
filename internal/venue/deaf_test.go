package venue

// A worker that stops answering mid-command.

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/wire"
)

// deafExecEnv makes the helper-process shim greet, take the tree, and then go
// silent the moment it is asked to run something — answering neither the
// command nor the cancel.
const deafExecEnv = "STEPS_TEST_DEAF_EXEC"

// deafUploadEnv makes it go silent one step earlier: it takes the tree and
// never says whether it landed.
const deafUploadEnv = "STEPS_TEST_DEAF_UPLOAD"

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

		switch frame.Type { //nolint:exhaustive // a stand-in shim answers only the frames its test sends
		case wire.FrameHello:
			_ = encoder.WriteJSON(wire.FrameHelloOK, frame.Op, wire.HelloOK{
				Protocol: wire.Protocol,
				Workdir:  os.TempDir(),
			})
		case wire.FrameEnd:
			if os.Getenv(deafUploadEnv) != "" {
				// Never acknowledge the tree: the shape a worker takes when
				// its disk stalls partway through writing one.
				continue
			}

			_ = encoder.Write(wire.Frame{Type: wire.FrameEnd, Op: frame.Op})
		case wire.FrameExec:
			// The whole point: no stdout, no exit, and no answer to the cancel
			// that follows. Keep reading so the connection stays up.
		case wire.FrameHelloOK, wire.FrameUpload, wire.FrameStdout, wire.FrameStderr,
			wire.FrameExit, wire.FrameFetch, wire.FrameData, wire.FrameCancel,
			wire.FrameError, wire.FrameBye, wire.FrameDraining:
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

// TestVenueCancellationEndsAnUnacknowledgedUpload is the same absent deadline,
// one step earlier in the session.
//
// Nothing runs until the worker says it took the tree — deliberately, since a
// command against a rejected tree has real side effects. But a worker that
// never answers is not a worker that refused, and that read had no deadline
// either: the build parked before its first command, where even the exec
// watchdog could not reach it.
func TestVenueCancellationEndsAnUnacknowledgedUpload(t *testing.T) {
	t.Setenv(deafExecEnv, "1")
	t.Setenv(deafUploadEnv, "1")

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	ctx, cancel := context.WithTimeout(context.Background(), shortWait)
	defer cancel()

	returned := make(chan error, 1)

	go func() { returned <- runner.Run(ctx, "anything") }()

	limit := time.NewTimer(30 * time.Second)
	defer limit.Stop()

	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("a step whose tree was never acknowledged reported success")
		}
	case <-limit.C:
		t.Fatal("the step never returned: the worker never acknowledged the tree and the read has no deadline, so the build waits on it forever")
	}
}

// TestVenueCancellationReachesAWedgedSSHWorker is the same wedge over the
// transport where the old enforcement quietly did nothing: an SSH stdout is a
// plain Reader in a NopCloser, so "close the reader" closed nothing, and a
// wedged ssh:// worker survived timeout:, fail_fast, race: and Ctrl-C — the
// exact failure the watchdog's own comment says it exists to end. The
// interrupt hook tears the SSH session itself down, which unblocks both
// directions.
func TestVenueCancellationReachesAWedgedSSHWorker(t *testing.T) {
	t.Setenv(deafExecEnv, "1")

	server := newTestSSHD(t)

	runner := newLocalRunner(t, sshSpec(t, server, t.TempDir()))

	ctx, cancel := context.WithTimeout(context.Background(), shortWait)
	defer cancel()

	returned := make(chan error, 1)

	go func() { returned <- runner.Run(ctx, "anything") }()

	limit := time.NewTimer(30 * time.Second)
	defer limit.Stop()

	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("a command a worker never answered reported success")
		}

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error = %v, want it to carry the deadline", err)
		}
	case <-limit.C:
		t.Fatal("the command never returned over ssh: the interrupt did not reach the wedged worker")
	}
}

// failingSink is a stream this end cannot write to: a stdout whose reader
// closed (`steps ... | head`), a capture spilling to a full disk.
type failingSink struct{ err error }

func (f failingSink) Write([]byte) (int, error) { return 0, f.err }

// TestRunDrainsToTheExitFrameWhenASinkFails is the seam between a LOCAL
// failure and the shared stream.
//
// A sink that cannot take a command's output used to end the read at once,
// leaving the rest of that operation's frames — and its FrameExit — in the
// stream. The session was not marked broken, so the next command reused it and
// met the leftovers as "a frame for operation N arrived during operation N+1":
// every later command of the step failed for a reason the worker had nothing
// to do with. pump() drains for exactly this reason on the transfer path; the
// command path had no equivalent.
func TestRunDrainsToTheExitFrameWhenASinkFails(t *testing.T) {
	t.Parallel()

	toVenue, fromShim := io.Pipe()

	session := &session{
		decoder: wire.NewDecoder(toVenue),
		encoder: wire.NewEncoder(io.Discard),
	}

	t.Cleanup(func() { _ = toVenue.Close() })

	go func() {
		shim := wire.NewEncoder(fromShim)

		// Two output frames, so the failure lands before the exit and there is
		// something left to abandon.
		_ = shim.Write(wire.Frame{Type: wire.FrameStdout, Op: 1, Payload: []byte("first")})
		_ = shim.Write(wire.Frame{Type: wire.FrameStdout, Op: 1, Payload: []byte("second")})
		_ = shim.WriteJSON(wire.FrameExit, 1, wire.Exit{Started: true, Code: 0})
		// The next operation's opening frame. If run stopped early, THIS is
		// not what the next read finds.
		_ = shim.Write(wire.Frame{Type: wire.FrameEnd, Op: 2})
	}()

	sinkErr := errors.New("the stream went away")

	_, err := session.run(context.Background(), "true",
		outputSinks{stdout: failingSink{err: sinkErr}, stderr: io.Discard})
	if !errors.Is(err, sinkErr) {
		t.Fatalf("error = %v, want the sink's own failure", err)
	}

	// The stream is back on the boundary the next operation starts at.
	next, err := session.readFrame()
	if err != nil {
		t.Fatalf("reading the next operation's frame: %v", err)
	}

	if next.Type != wire.FrameEnd || next.Op != 2 {
		t.Errorf("the next frame was a type %d for operation %d, want the next operation's own — the session is desynced",
			next.Type, next.Op)
	}
}
