package venue

// A worker running a binary this run did not push.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// wrongBuildEnv makes the helper-process shim answer the handshake with a
// build hash that is not the one it was started as.
const wrongBuildEnv = "STEPS_TEST_WRONG_BUILD"

// serveWrongBuildShim speaks the protocol correctly and is simply not the
// binary that was pushed: an older steps left at the path by hand, or an
// upload whose truncation the size check happened to miss.
func serveWrongBuildShim() {
	decoder := wire.NewDecoder(os.Stdin)
	encoder := wire.NewEncoder(os.Stdout)

	for {
		frame, err := decoder.Read()
		if err != nil {
			return
		}

		if frame.Type == wire.FrameHello {
			_ = encoder.WriteJSON(wire.FrameHelloOK, frame.Op, wire.HelloOK{
				Protocol: wire.Protocol,
				Build:    "0000000000000000000000000000000000000000000000000000000000000000",
				Workdir:  os.TempDir(),
			})
		}
	}
}

// TestVenueRefusesAWorkerRunningAnotherBuild is the check wire.Hello.Build has
// always described and nobody performed.
//
// A pushed binary is reused when a file of the right SIZE is already at its
// content-keyed path, which is a guess about bytes rather than proof of them.
// The shim reports its own build in the handshake precisely so the guess can
// be checked — and the answer was read into a struct and dropped, so a worker
// running the wrong steps ran the step anyway and reported whatever that
// binary did.
func TestVenueRefusesAWorkerRunningAnotherBuild(t *testing.T) {
	t.Setenv(wrongBuildEnv, "1")

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	err := runner.Run(context.Background(), "true")
	if err == nil {
		t.Fatal("a worker running a different binary ran the step")
	}

	if !errors.Is(err, errWrongBuild) {
		t.Fatalf("error = %v, want it to name the build mismatch", err)
	}

	if !strings.Contains(err.Error(), "000000000000") {
		t.Errorf("error = %v, want the build the worker reported, so an operator can tell which machine to clean up", err)
	}
}
