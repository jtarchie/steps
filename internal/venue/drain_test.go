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
	mode := os.Getenv(drainingShimEnv)
	decoder := wire.NewDecoder(os.Stdin)
	encoder := wire.NewEncoder(os.Stdout)

	notice := wire.Draining{
		Reason:   "EC2 spot terminate",
		Deadline: "2026-08-25T12:00:00Z",
		Terminal: true,
	}
	if mode == "advisory" {
		notice = wire.Draining{Reason: "EC2 rebalance recommendation"}
	}

	for {
		frame, err := decoder.Read()
		if err != nil {
			return
		}

		switch frame.Type { //nolint:exhaustive // a stub with opinions about four frames
		case wire.FrameHello:
			_ = encoder.WriteJSON(wire.FrameHelloOK, frame.Op, wire.HelloOK{
				Protocol: wire.Protocol, Workdir: os.TempDir(),
			})
		case wire.FrameUpload:
			if mode == "upload" {
				// The likeliest timing of all: the notice lands while the
				// tree is still arriving, which is the longest phase.
				_ = encoder.WriteJSON(wire.FrameDraining, wire.DrainOp, notice)
			}

			// An upload is an OFFER now: the worker says whether it needs the
			// bytes. This stand-in always does, which keeps the tree arriving
			// as the timing above depends on.
			_ = encoder.Write(wire.Frame{Type: wire.FrameNeed, Op: frame.Op})
		case wire.FrameEnd:
			_ = encoder.Write(wire.Frame{Type: wire.FrameEnd, Op: frame.Op})
		case wire.FrameExec:
			drainingExec(encoder, frame, mode, notice)
		}
	}
}

// drainingExec answers one command the way the mode under test needs.
func drainingExec(encoder *wire.Encoder, frame wire.Frame, mode string, notice wire.Draining) {
	if mode == "upload" {
		// Nothing left to prove: the session survived a notice sent during
		// the tree upload, so run the command like any healthy worker.
		_ = encoder.WriteJSON(wire.FrameExit, frame.Op, wire.Exit{Started: true, Code: 0})

		return
	}

	_ = encoder.WriteJSON(wire.FrameDraining, wire.DrainOp, notice)

	switch mode {
	case "signal":
		// The shape a real reclamation usually takes: the machine signals the
		// command, so an exit frame DOES arrive carrying the signalled
		// sentinel.
		_ = encoder.WriteJSON(wire.FrameExit, frame.Op, wire.Exit{Started: true, Code: -1})
	case "verdict":
		// The command chose its own status before the machine went.
		_ = encoder.WriteJSON(wire.FrameExit, frame.Op, wire.Exit{Started: true, Code: 3})
	}

	os.Exit(1)
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

// TestVenueReadsASignalledExitOnADrainedWorkerAsEviction is the shape a real
// reclamation usually takes, and the one a narrower rule missed: the instance
// runs its shutdown, init signals the command, and the shim reports a
// started-and-signalled exit. That is the machine ending the command rather
// than the command answering, so it has to be infrastructure — otherwise the
// commonest eviction of all fails the build as the step's own verdict.
func TestVenueReadsASignalledExitOnADrainedWorkerAsEviction(t *testing.T) {
	t.Setenv(drainingShimEnv, "signal")

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	err := runner.Run(context.Background(), "echo never-runs")
	if err == nil {
		t.Fatal("a signalled command on a reclaimed worker reported success")
	}

	if !errors.Is(err, ErrEvicted) {
		t.Fatalf("error = %v, want ErrEvicted", err)
	}
}

// TestVenueKeepsACommandsOwnVerdictOnADrainedWorker is the other half of that
// line: a command that RAN and chose a nonzero status said something about
// the step, and the machine going away afterwards does not unsay it.
// Re-placing there would repeat work whose answer was already given.
func TestVenueKeepsACommandsOwnVerdictOnADrainedWorker(t *testing.T) {
	t.Setenv(drainingShimEnv, "verdict")

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	err := runner.Run(context.Background(), "exit 3")
	if err == nil {
		t.Fatal("a failing command reported success")
	}

	if errors.Is(err, ErrEvicted) {
		t.Fatalf("a command's own nonzero exit was re-read as an eviction: %v", err)
	}

	if !shell.IsExitError(err) {
		t.Fatalf("error = %v, want the command's own exit", err)
	}
}

// TestVenueDoesNotTreatAnAdvisoryNoticeAsAReclamation pins the distinction
// EC2 draws and this end must not flatten: a rebalance recommendation is a
// hint that need never be followed by an interruption. Acting on it the way a
// reclamation is acted on would terminate a healthy machine the job paid to
// acquire and re-run a step that had nothing wrong with it.
func TestVenueDoesNotTreatAnAdvisoryNoticeAsAReclamation(t *testing.T) {
	t.Setenv(drainingShimEnv, "advisory")

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	err := runner.Run(context.Background(), "echo never-runs")
	if err == nil {
		t.Fatal("the stub reported success")
	}

	if errors.Is(err, ErrEvicted) {
		t.Fatalf("an advisory rebalance notice armed the eviction path: %v", err)
	}
}

// TestVenueAbsorbsADrainNoticeDuringATransfer is the failure the notice's own
// timing makes likeliest: the worker learns of its end while the step's tree
// is still going out, which is the longest phase of a session. Handled per
// call site, that frame was reported as a protocol violation and poisoned the
// session with an invented error — on exactly the machines the feature exists
// for.
func TestVenueAbsorbsADrainNoticeDuringATransfer(t *testing.T) {
	t.Setenv(drainingShimEnv, "upload")

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")

	runner := newLocalRunner(t, localWorker(t, cwd))

	err := runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("a drain notice during the tree upload broke the session: %v", err)
	}
}

// TestVenueGuardShapedCallReportsAnEviction pins executeFull's half of the
// classification. An eviction can WEAR an exit — the shutdown signals the
// command and the shim reports started-with-code-minus-one — and errors.As
// sees that ExitError straight through the ErrEvicted wrap. Converted to
// exit-as-data there, a placed when: guard would read the machine dying as
// the guard answering no, and silently skip the work it gates.
func TestVenueGuardShapedCallReportsAnEviction(t *testing.T) {
	t.Setenv(drainingShimEnv, "signal")

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	_, _, _, err := runner.RunCaptureFull(context.Background(), "echo never-runs")
	if err == nil {
		t.Fatal("a guard-shaped call on a reclaimed worker returned its exit as data")
	}

	if !errors.Is(err, ErrEvicted) {
		t.Fatalf("error = %v, want ErrEvicted", err)
	}
}

// TestVenueGuardShapedCallKeepsARealVerdict is the boundary: a command that
// chose its status is data, drained machine or not — a guard exit of 3 is
// the guard answering, and must not become an error.
func TestVenueGuardShapedCallKeepsARealVerdict(t *testing.T) {
	t.Setenv(drainingShimEnv, "verdict")

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	_, _, code, err := runner.RunCaptureFull(context.Background(), "exit 3")
	if err != nil {
		t.Fatalf("a command's own verdict was reported as an error: %v", err)
	}

	if code != 3 {
		t.Errorf("code = %d, want the guard's own answer", code)
	}
}
