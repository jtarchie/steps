package shell

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// requireDocker skips the calling test unless a usable Docker daemon is
// reachable. Not opt-in: a test guarding a shipped feature does not get to be
// optional, and one that was is how a containerized placed step shipped not
// working. A test that BIND-MOUNTS needs mountableTempDir as well — the
// daemon may be in a VM that cannot see this process's temp directory.
func requireDocker(t *testing.T) {
	t.Helper()

	_, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not found on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = exec.CommandContext(ctx, "docker", "info").Run()
	if err != nil {
		t.Skip("docker daemon not reachable (`docker info` failed)")
	}
}

const testImage = "alpine:3"

// mountableTempDir returns a temp directory the docker daemon can actually
// see, for a test that bind-mounts one.
//
// t.TempDir() is not good enough, and the reason is worth stating because the
// failure is silent. When the daemon runs in a VM (Docker Desktop, colima)
// only some host paths are shared into it: the user's home is, macOS's own
// $TMPDIR (/var/folders/...) is not. Bind-mounting an unshared path does not
// error — docker creates an empty directory at the mount target — so the
// container writes happily into a phantom directory and the host sees
// nothing. A real run is under the same constraint; see docs/infra.md on
// TMPDIR.
func mountableTempDir(t *testing.T) string {
	t.Helper()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to root a daemon-visible temp dir in")
	}

	dir, err := os.MkdirTemp(home, ".steps-test-*")
	if err != nil {
		t.Skipf("cannot create a daemon-visible temp dir under %s: %v", home, err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

// TestDockerRunnerIntegrationBindMountPersists and its siblings below are
// split into separate top-level functions (rather than t.Run subtests of
// one function) to stay under the linter's per-function
// cyclomatic-complexity budget.
func TestDockerRunnerIntegrationBindMountPersists(t *testing.T) {
	requireDocker(t)

	dir := mountableTempDir(t)

	runner, err := NewRunner(RunnerSpec{Image: testImage, Cwd: dir})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	err = runner.Run(context.Background(), "echo hello > written.txt")
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
	requireDocker(t)

	runner, err := NewRunner(RunnerSpec{Image: testImage, Cwd: mountableTempDir(t)})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	_, _, exitCode, err := runner.RunCaptureFull(context.Background(), "exit 7")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	if exitCode != 7 {
		t.Errorf("exitCode = %d, want 7", exitCode)
	}
}

func TestDockerRunnerIntegrationHostEnvNotVisible(t *testing.T) {
	requireDocker(t)

	t.Setenv("STEPS_TEST_HOST_SECRET", "leak-me-not")

	runner, err := NewRunner(RunnerSpec{Image: testImage, Cwd: mountableTempDir(t)})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	stdout, _, _, err := runner.RunCaptureFull(context.Background(), "echo \"[$STEPS_TEST_HOST_SECRET]\"")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	if stdout != "[]\n" {
		t.Errorf("stdout = %q, want the host env var to be absent inside the container", stdout)
	}
}

func TestDockerRunnerIntegrationCancellationTerminatesWithinGrace(t *testing.T) {
	requireDocker(t)

	runner, err := NewRunner(RunnerSpec{Image: testImage, Cwd: mountableTempDir(t)})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()

	_, _, _, runErr := runner.RunCaptureFull(ctx, "sleep 30")

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
	requireDocker(t)

	runner, err := NewRunner(RunnerSpec{Image: testImage, Cwd: mountableTempDir(t)})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	// Written outside the bind mount on purpose: a file under the mounted
	// working directory would persist even without a shared container, so it
	// would prove nothing about the container being the same one.
	_, _, exitCode, err := runner.RunCaptureFull(context.Background(), "echo persisted > /tmp/marker")
	if err != nil {
		t.Fatalf("RunCaptureFull (write): %v", err)
	}

	if exitCode != 0 {
		t.Fatalf("writing the marker exited %d", exitCode)
	}

	stdout, stderr, exitCode, err := runner.RunCaptureFull(context.Background(), "cat /tmp/marker")
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
	requireDocker(t)

	runnerIface, err := NewRunner(RunnerSpec{Image: testImage, Cwd: mountableTempDir(t)})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	runner, ok := runnerIface.(DockerRunner)
	if !ok {
		t.Fatal("expected a DockerRunner")
	}

	_, _, _, err = runner.RunCaptureFull(context.Background(), "true")
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
	requireDocker(t)

	err := ValidateDocker(context.Background())
	if err != nil {
		t.Errorf("ValidateDocker: %v", err)
	}
}
