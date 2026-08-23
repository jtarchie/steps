package venue

// A worker that refuses the step's tree.

import (
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

		switch frame.Type {
		case wire.FrameHello:
			_ = encoder.WriteJSON(wire.FrameHelloOK, frame.Op, wire.HelloOK{
				Protocol: wire.Protocol,
				Workdir:  os.TempDir(),
			})
		case wire.FrameEnd:
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
		case wire.FrameHelloOK, wire.FrameUpload, wire.FrameStdout, wire.FrameStderr,
			wire.FrameExit, wire.FrameFetch, wire.FrameData, wire.FrameCancel,
			wire.FrameError, wire.FrameBye:
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
