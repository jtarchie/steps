package venue

// A worker that announces its own end.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/wire"
)

// drainingShimEnv makes the helper-process shim announce an eviction and then
// die, which is what a reclaimed spot instance does.
const drainingShimEnv = "STEPS_TEST_DRAINING_SHIM"

// serveDrainingShim greets, accepts a tree, and then — instead of running the
// command — says it is being reclaimed and goes away. A real eviction leaves
// a two-minute window; this takes the harsher shape on purpose, because the
// classification is what is under test and the harsh shape is the one that
// used to be read as a step failure.
func serveDrainingShim() {
	decoder := wire.NewDecoder(os.Stdin)
	encoder := wire.NewEncoder(os.Stdout)

	for {
		frame, err := decoder.Read()
		if err != nil {
			return
		}

		switch frame.Type { //nolint:exhaustive // a stub with opinions about three frames
		case wire.FrameHello:
			_ = encoder.WriteJSON(wire.FrameHelloOK, frame.Op, wire.HelloOK{
				Protocol: wire.Protocol, Workdir: os.TempDir(),
			})
		case wire.FrameUpload:
		case wire.FrameEnd:
			_ = encoder.Write(wire.Frame{Type: wire.FrameEnd, Op: frame.Op})
		case wire.FrameExec:
			_ = encoder.WriteJSON(wire.FrameDraining, wire.DrainOp, wire.Draining{
				Reason:   "EC2 spot terminate",
				Deadline: "2026-08-25T12:00:00Z",
			})

			os.Exit(1)
		}
	}
}

// TestVenueReportsAnEvictionAsInfrastructure is the divergence, at this
// layer: a worker taken away underneath a step fails it as ErrEvicted, never
// as the command's own verdict — so the pipeline can re-place it without
// spending the author's attempts: budget.
func TestVenueReportsAnEvictionAsInfrastructure(t *testing.T) {
	t.Setenv(drainingShimEnv, "1")

	cwd := t.TempDir()
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, localWorker(t, cwd, "out"))

	err := runner.Run(context.Background(), "echo never-runs")
	if err == nil {
		t.Fatal("a step on a reclaimed worker reported success")
	}

	if !errors.Is(err, ErrEvicted) {
		t.Fatalf("error = %v, want ErrEvicted", err)
	}

	// The distinction the whole feature turns on: an eviction must never look
	// like the command having run and failed.
	if shell.IsExitError(err) {
		t.Errorf("an eviction was reported as a command's exit: %v", err)
	}

	// The cloud's own words, so a build says why rather than just that.
	if !strings.Contains(err.Error(), "EC2 spot terminate") {
		t.Errorf("error = %v, want the reason the worker gave", err)
	}
}
