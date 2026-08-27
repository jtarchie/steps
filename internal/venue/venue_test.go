package venue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

// localWorker points a spec at this test binary running as a shim. See
// TestMain: under `go test` the binary a local: venue would exec is the test
// binary itself, which does not answer to _shim unless something dispatches
// it.
func localWorker(t *testing.T, cwd string, outputs ...string) shell.RunnerSpec {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	return shell.RunnerSpec{
		Cwd:    cwd,
		Worker: "local:?binary=" + self,
		Fetch:  outputs,
	}
}

func newLocalRunner(t *testing.T, spec shell.RunnerSpec) shell.Runner {
	t.Helper()

	runner, err := NewRunner(spec)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	return runner
}

// TestVenueRunsACommandAndBringsOutputsBack is the whole contract in one test:
// the tree goes out, the command runs against it, and what it produced is on
// this machine by the time the call returns.
func TestVenueRunsACommandAndBringsOutputsBack(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, localWorker(t, cwd, "out"))

	err := runner.Run(context.Background(), "cat data/seed.txt > out/report.txt")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Before Capture, not after: this is what an assert: files: on a placed
	// step reads, and it reads it inside the retry loop.
	got := mustRead(t, filepath.Join(cwd, "out", "report.txt"))
	if got != "seed\n" {
		t.Errorf("out/report.txt = %q, want %q", got, "seed\n")
	}
}

// TestVenueNonzeroExitIsAStepFailure is the classification the whole feature
// turns on. A command that ran and failed has to look like a failed step, not
// like broken machinery, or hooks fire on the wrong event.
func TestVenueNonzeroExitIsAStepFailure(t *testing.T) {
	t.Parallel()

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	err := runner.Run(context.Background(), "exit 3")
	if err == nil {
		t.Fatal("Run succeeded on a command that exited 3")
	}

	if !shell.IsExitError(err) {
		t.Errorf("IsExitError = false for a remote nonzero exit: %v — this would fire on_error where the author wrote on_failure", err)
	}
}

// TestVenueReportsExitAsData covers the other contract: RunCaptureFull hands
// back a nonzero exit as a number, and errors only when nothing ran.
func TestVenueReportsExitAsData(t *testing.T) {
	t.Parallel()

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	stdout, stderr, code, err := runner.RunCaptureFull(context.Background(), "echo out; echo err >&2; exit 4")
	if err != nil {
		t.Fatalf("RunCaptureFull returned an error for a command that ran: %v", err)
	}

	if code != 4 {
		t.Errorf("exit code = %d, want 4", code)
	}

	if stdout != "out\n" || stderr != "err\n" {
		t.Errorf("streams = %q / %q, want %q / %q", stdout, stderr, "out\n", "err\n")
	}
}

// TestVenueUnreachableWorkerIsNotAVerdict is the inverse, and the one that
// protects guards. A worker that cannot be reached must never look like a
// command that answered.
func TestVenueUnreachableWorkerIsNotAVerdict(t *testing.T) {
	t.Parallel()

	spec := shell.RunnerSpec{Cwd: t.TempDir(), Worker: "local:?binary=" + filepath.Join(t.TempDir(), "no-such-binary")}

	runner, err := NewRunner(spec)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	_, _, _, err = runner.RunCaptureFull(context.Background(), "true")
	if err == nil {
		t.Fatal("RunCaptureFull succeeded against a worker that cannot be started")
	}

	if shell.IsExitError(err) {
		t.Error("an unreachable worker classified as a command's own exit — a guard would read this as 'no' and skip the work it gates")
	}
}

// TestVenueCapturesAndStreams pins the two halves of RunStreamedCapture.
func TestVenueCapturesAndStreams(t *testing.T) {
	t.Parallel()

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	stdout, _, err := runner.RunStreamedCapture(context.Background(), "echo captured", 0)
	if err != nil {
		t.Fatalf("RunStreamedCapture: %v", err)
	}

	if stdout != "captured\n" {
		t.Errorf("stdout = %q, want %q", stdout, "captured\n")
	}
}

// TestVenueBoundsCapturedOutput pins that the orchestrator applies the same
// cap it applies locally, rather than trusting the worker to have done it.
func TestVenueBoundsCapturedOutput(t *testing.T) {
	t.Parallel()

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	stdout, _, _, err := runner.RunCaptureFullLimited(context.Background(), "printf '0123456789'", 4, "")
	if err != nil {
		t.Fatalf("RunCaptureFullLimited: %v", err)
	}

	if !strings.HasPrefix(stdout, "0123") {
		t.Errorf("stdout = %q, want it to start with the first 4 bytes", stdout)
	}

	if !strings.Contains(stdout, "truncated") {
		t.Errorf("stdout = %q, want a truncation marker", stdout)
	}
}

// TestVenueSharesOneSessionAcrossLabels pins the pointer-sharing DockerRunner
// established: a labelled copy talks to the same worker, and closing either
// closes both.
func TestVenueSharesOneSessionAcrossLabels(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, localWorker(t, cwd, "out"))
	labelled := runner.WithLabel("build")

	err := runner.Run(context.Background(), "echo one > out/one.txt")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The second command runs in the SAME work directory on the worker, which
	// is only true if the labelled copy shares the session rather than dialing
	// its own.
	err = labelled.Run(context.Background(), "test -f out/one.txt && echo two > out/two.txt")
	if err != nil {
		t.Fatalf("the labelled copy did not see the first command's work: %v", err)
	}

	mustRead(t, filepath.Join(cwd, "out", "two.txt"))
}

// TestVenueCloseIsSafeWhenNothingRan covers the ordinary path for a step that
// failed or was skipped before its first command.
func TestVenueCloseIsSafeWhenNothingRan(t *testing.T) {
	t.Parallel()

	runner, err := NewRunner(localWorker(t, t.TempDir()))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	err = runner.Close()
	if err != nil {
		t.Fatalf("Close on an unused runner: %v", err)
	}

	// Twice, because CloseRunner runs from deferred paths that can overlap.
	err = runner.Close()
	if err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestVenueCancellationStopsTheCommand pins that a cancelled context reaches a
// process on the other side of the wire — the thing SSH signals cannot do.
func TestVenueCancellationStopsTheCommand(t *testing.T) {
	t.Parallel()

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	ctx, cancel := context.WithTimeout(context.Background(), shortWait)
	defer cancel()

	err := runner.Run(ctx, "sleep 60")
	if err == nil {
		t.Fatal("a cancelled command reported success")
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to carry the deadline — callers tell an interrupted step from a failed one by asking", err)
	}
}

// TestVenueRefusesAnImage pins the load-time rule at the one place a
// hand-built spec could still get past it.
// TestVenueCarriesAnImageToTheWorker pins that a placed step which names an
// image is accepted and keeps its container settings.
//
// This used to be a refusal — a worker ran a step's commands directly, and a
// container on the worker meant bind-mounting a tree that had just been sent.
// It now means exactly that, so the spec has to survive: the runner builds
// its container from it once the handshake reports where the tree landed.
func TestVenueCarriesAnImageToTheWorker(t *testing.T) {
	t.Parallel()

	built, err := NewRunner(shell.RunnerSpec{
		Cwd: t.TempDir(), Worker: "local:", Image: "alpine", Network: "none",
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = built.Close() })

	placed, ok := built.(runner)
	if !ok {
		t.Fatalf("runner is %T, not a placed one", built)
	}

	if placed.session.container.Image != "alpine" {
		t.Errorf("image = %q, want the step's image carried to the worker", placed.session.container.Image)
	}

	if placed.session.container.Network != "none" {
		t.Errorf("network = %q, want the container settings carried with it", placed.session.container.Network)
	}
}

// TestNoWorkerIsTheLocalRunner pins that an untagged step takes exactly the
// path it took before this package existed.
func TestNoWorkerIsTheLocalRunner(t *testing.T) {
	t.Parallel()

	runner, err := NewRunner(shell.RunnerSpec{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer func() { _ = runner.Close() }()

	// By the shape ReclaimedBy actually asserts, because that is the one a
	// venue runner really has. The assertion here used to name
	// isVenueRunner(), a method no type in this repo implements — so it was
	// false for both answers and the test could not fail however NewRunner
	// was broken.
	if _, remote := runner.(interface{ reclaimed() (string, bool) }); remote {
		t.Error("a spec with no worker produced a venue runner")
	}

	err = runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("the local runner did not run: %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0o750)
	if err != nil {
		t.Fatalf("mkdir for %q: %v", path, err)
	}

	err = os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()

	err := os.MkdirAll(path, 0o750)
	if err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path) //nolint:gosec // a path this test just built
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}

	return string(content)
}
