package venue

// A worker that refuses the step's tree.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/wire"
)

// rejectUploadEnv makes the helper-process shim answer the hello and then
// refuse whatever tree it is sent.
const rejectUploadEnv = "STEPS_TEST_REJECT_UPLOAD"

// breakFetchEnv makes it accept everything and then answer the fetch with
// bytes that are not an archive, so the transfer fails after the orchestrator
// has committed to it.
const breakFetchEnv = "STEPS_TEST_BREAK_FETCH"

// serveRejectingShim is the far end of that: it greets normally, drains the
// upload, and reports it failed — the shape a worker takes when its disk is
// full or its scratch is read-only.
func serveRejectingShim() {
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
		case wire.FrameFetch:
			// Not an archive. UnpackTree fails, which is every way a transfer
			// can die after the orchestrator asked for it: a dropped
			// connection, a truncated stream, a worker that went away. MORE
			// THAN ONE frame, deliberately: the unpacker gives up while
			// later frames are still in flight, which is what forces the
			// orchestrator to drain to the operation's end rather than
			// leave them for the next command to misread.
			// A full header block of garbage, so the unpacker fails on frame
			// one rather than waiting for more bytes; the second frame is
			// then the one an early-returning reader would leave behind.
			_ = encoder.Write(wire.Frame{Type: wire.FrameData, Op: frame.Op, Payload: bytes.Repeat([]byte("not a tar stream "), 64)})
			_ = encoder.Write(wire.Frame{Type: wire.FrameData, Op: frame.Op, Payload: []byte("and still not one")})
			_ = encoder.Write(wire.Frame{Type: wire.FrameEnd, Op: frame.Op})
		case wire.FrameUpload:
			// Asked for, because the refusal this stub exists to test happens
			// at the END of a tree it accepted the offer of.
			_ = encoder.Write(wire.Frame{Type: wire.FrameNeed, Op: frame.Op})
		case wire.FrameEnd:
			if os.Getenv(breakFetchEnv) != "" {
				// This mode takes the tree; only the fetch is broken.
				_ = encoder.Write(wire.Frame{Type: wire.FrameEnd, Op: frame.Op})

				continue
			}

			// The end of the tree it was refusing.
			_ = encoder.WriteJSON(wire.FrameError, frame.Op, wire.Error{
				Message: "unpacking the step tree: no space left on device",
			})
		case wire.FrameExec:
			// Exactly what the real shim does after reporting a failed
			// operation: it reports and CONTINUES, so the next frame is
			// handled normally — the command runs. That is the bug.
			var request wire.Exec

			_ = wire.DecodeJSON(frame, &request)

			_ = exec.CommandContext(context.Background(), "sh", "-c", request.Command).Run() //nolint:gosec // a test stub running the command under test

			_ = encoder.WriteJSON(wire.FrameExit, frame.Op, wire.Exit{Started: true, Code: 0})
		case wire.FrameHelloOK, wire.FrameStdout, wire.FrameStderr,
			wire.FrameExit, wire.FrameData, wire.FrameCancel,
			wire.FrameError, wire.FrameBye, wire.FrameDraining:
			// Everything else this stub has no opinion about, including the
			// data frames of the tree it is refusing.
		}
	}
}

// TestVenueRefusedUploadNeverRunsTheCommand is the contract: a worker that
// could not take the tree must not then be asked to run against it.
//
// The command has real side effects — it pushes, deploys, deletes — so running
// it against a half-unpacked tree is worse than any transfer error. The
// orchestrator used to send its upload frames, check only its own writes, and
// go straight on to exec.
func TestVenueRefusedUploadNeverRunsTheCommand(t *testing.T) {
	t.Setenv(rejectUploadEnv, "1")

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")

	marker := filepath.Join(cwd, "RAN-ANYWAY")

	runner := newLocalRunner(t, localWorker(t, cwd))

	err := runner.Run(context.Background(), "touch "+marker)
	if err == nil {
		t.Fatal("the step succeeded against a worker that refused its tree")
	}

	if !strings.Contains(err.Error(), "no space left on device") {
		t.Errorf("error = %v, want the worker's own reason for refusing", err)
	}

	// The child may still be mid-touch when the transport closes, so give the
	// side effect every chance to appear before claiming it did not.
	time.Sleep(500 * time.Millisecond)

	_, statErr := os.Stat(marker)
	t.Logf("marker %q stat: %v", marker, statErr)

	if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("the command ran anyway — a rejected tree still had side effects on the worker")
	}
}

// TestVenueFailedFetchKeepsTheLocalOutputs pins that a transfer which dies
// leaves the step's outputs where they were.
//
// fetch() used to os.RemoveAll every declared output BEFORE reading a byte of
// the reply, so a dropped connection destroyed them irrecoverably — and the
// retry that follows re-uploads the now-emptied tree, so the second attempt
// fails for a reason unrelated to the first. Local execution has no such
// window.
func TestVenueFailedFetchKeepsTheLocalOutputs(t *testing.T) {
	t.Setenv(breakFetchEnv, "1")

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "out", "previous.txt"), "from an earlier attempt\n")

	runner := newLocalRunner(t, localWorker(t, cwd, "out"))

	err := runner.Run(context.Background(), "true")
	if err == nil {
		t.Fatal("the step succeeded despite a fetch that could not be unpacked")
	}

	kept := filepath.Join(cwd, "out", "previous.txt")

	_, statErr := os.Stat(kept)
	if statErr != nil {
		t.Fatalf("a failed fetch destroyed the local outputs: %v", statErr)
	}
}

// TestVenueSurvivesABrokenFetch pins that a fetch the orchestrator cannot
// unpack costs that command and nothing after it. The shim is still SENDING
// the operation's frames when the unpack gives up, and it resumes its own
// loop cleanly — so an orchestrator that stopped reading mid-operation left
// those frames in the stream, and the next command read them as a protocol
// violation: a session poisoned by its own tidiness, every retry desynced,
// while the shim executed each retried command and had its answers misread.
func TestVenueSurvivesABrokenFetch(t *testing.T) {
	t.Setenv(breakFetchEnv, "1")

	cwd := t.TempDir()
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, localWorker(t, cwd, "out"))

	err := runner.Run(context.Background(), "true")
	if err == nil {
		t.Fatal("the step succeeded despite a fetch that could not be unpacked")
	}

	// The next command reaches the same session on a clean frame boundary.
	err = runner.Run(context.Background(), "true")
	if err == nil || strings.Contains(err.Error(), "arrived during operation") {
		t.Fatalf("the command after a broken fetch hit a desynced session: %v", err)
	}
}
