package shell

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/testdocker"
)

const testImage = "alpine:3"

// TestDockerRunnerIntegrationBindMountPersists and its siblings below are
// split into separate top-level functions (rather than t.Run subtests of
// one function) to stay under the linter's per-function
// cyclomatic-complexity budget.
func TestDockerRunnerIntegrationBindMountPersists(t *testing.T) {
	testdocker.Require(t)

	dir := testdocker.MountableDir(t)

	runner, err := NewRunner(RunnerSpec{Image: testImage, Cwd: dir})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	err = Run(context.Background(), runner, "echo hello > written.txt")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, readErr := os.ReadFile(filepath.Join(dir, "written.txt")) //nolint:gosec // test fixture path
	if readErr != nil {
		t.Fatalf("read file written inside the container: %v", readErr)
	}

	if string(data) != "hello\n" {
		t.Errorf("content = %q, want %q", data, "hello\n")
	}
}

func TestDockerRunnerIntegrationExitCodeRoundTrips(t *testing.T) {
	testdocker.Require(t)

	runner, err := NewRunner(RunnerSpec{Image: testImage, Cwd: testdocker.MountableDir(t)})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	_, _, exitCode, err := RunCaptureFull(context.Background(), runner, "exit 7")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	if exitCode != 7 {
		t.Errorf("exitCode = %d, want 7", exitCode)
	}
}

func TestDockerRunnerIntegrationHostEnvNotVisible(t *testing.T) {
	testdocker.Require(t)

	t.Setenv("STEPS_TEST_HOST_SECRET", "leak-me-not")

	runner, err := NewRunner(RunnerSpec{Image: testImage, Cwd: testdocker.MountableDir(t)})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	stdout, _, _, err := RunCaptureFull(context.Background(), runner, "echo \"[$STEPS_TEST_HOST_SECRET]\"")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	if stdout != "[]\n" {
		t.Errorf("stdout = %q, want the host env var to be absent inside the container", stdout)
	}
}

func TestDockerRunnerIntegrationCancellationTerminatesWithinGrace(t *testing.T) {
	testdocker.Require(t)

	runner, err := NewRunner(RunnerSpec{Image: testImage, Cwd: testdocker.MountableDir(t)})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	// The container is started first, untimed: a cancel that lands during create waits out createBound by design (a cancelled create still has to remove what it made), and a loaded daemon put the 500ms there and measured 22s.
	_, _, _, err = RunCaptureFull(t.Context(), runner, "true")
	if err != nil {
		t.Fatalf("starting the session: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()

	_, _, _, runErr := RunCaptureFull(ctx, runner, "sleep 30")

	// The command itself keeps running in the container until the session is
	// torn down — the same as before, when the thing being killed was a docker
	// client rather than a read. What has to end promptly is the WAIT, because
	// a cancel is usually racing the very thing it means to stop, and a wait
	// that outlived it would be worse than useless. Bounded well under the
	// command's own 30s, so a cancel that did nothing fails here.
	const bound = 10 * time.Second

	elapsed := time.Since(start)
	if elapsed > bound {
		t.Errorf("took %s to return after cancellation, want well under %s", elapsed, bound)
	}

	_ = runErr // the reported status of a cut-off command varies; only timing is asserted here
}

// TestDockerRunnerIntegrationStatePersistsAcrossCommands is the behavior the
// per-step container exists for, against a real daemon: an agent's
// `pip install x` followed by `python y` as two run_shell calls has to work.
// Under the old fresh-container-per-command shape every assertion here failed.
func TestDockerRunnerIntegrationStatePersistsAcrossCommands(t *testing.T) {
	testdocker.Require(t)

	runner, err := NewRunner(RunnerSpec{Image: testImage, Cwd: testdocker.MountableDir(t)})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	// Written outside the bind mount on purpose: a file under the mounted
	// working directory would persist even without a shared container, so it
	// would prove nothing about the container being the same one.
	_, _, exitCode, err := RunCaptureFull(context.Background(), runner, "echo persisted > /tmp/marker")
	if err != nil {
		t.Fatalf("RunCaptureFull (write): %v", err)
	}

	if exitCode != 0 {
		t.Fatalf("writing the marker exited %d", exitCode)
	}

	stdout, stderr, exitCode, err := RunCaptureFull(context.Background(), runner, "cat /tmp/marker")
	if err != nil {
		t.Fatalf("RunCaptureFull (read): %v", err)
	}

	if exitCode != 0 {
		t.Fatalf("reading the marker exited %d (stderr: %s) — the second command ran in a different container", exitCode, stderr)
	}

	if stdout != "persisted\n" {
		t.Errorf("stdout = %q, want %q", stdout, "persisted\n")
	}
}

// TestDockerRunnerIntegrationCloseRemovesContainer confirms against a real
// daemon that a step leaves nothing running behind it — the orphaned-container
// caveat the session shape was meant to retire.
func TestDockerRunnerIntegrationCloseRemovesContainer(t *testing.T) {
	testdocker.Require(t)

	runnerIface, err := NewRunner(RunnerSpec{Image: testImage, Cwd: testdocker.MountableDir(t)})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	runner, ok := runnerIface.(DockerRunner)
	if !ok {
		t.Fatal("expected a DockerRunner")
	}

	_, _, _, err = RunCaptureFull(context.Background(), runner, "true")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	name := runner.session.name
	if name == "" {
		t.Fatal("expected the session to have named a container")
	}

	err = runner.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "name="+name).Output() //nolint:gosec // name is a hex string this package generated
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}

	if len(bytes.TrimSpace(out)) != 0 {
		t.Errorf("container %s still exists after Close (docker ps: %q)", name, out)
	}
}

func TestValidateDockerIntegration(t *testing.T) {
	testdocker.Require(t)

	err := ValidateDocker(context.Background())
	if err != nil {
		t.Errorf("ValidateDocker: %v", err)
	}
}
