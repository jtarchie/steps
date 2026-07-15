package shell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// requireDocker skips the calling test unless it's explicitly opted into and
// a usable Docker daemon is reachable. Mirrors internal/workspace's
// docker_btrfs_test.go precedent: heavyweight/non-hermetic tests (pulls an
// image) must not run as part of a plain `go test ./...`.
func requireDocker(t *testing.T) {
	t.Helper()

	if os.Getenv("STEPS_TEST_DOCKER") == "" {
		t.Skip("set STEPS_TEST_DOCKER=1 to run the Docker-backed shell tests (heavyweight: pulls an image)")
	}

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

func TestDockerRunnerIntegration(t *testing.T) {
	requireDocker(t)

	t.Run("a bind-mounted write persists on the host", func(t *testing.T) {
		dir := t.TempDir()

		err := DockerRunner{Image: testImage}.Run(context.Background(), "echo hello > written.txt", dir)
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
	})

	t.Run("exit code round-trips through RunCaptureFull", func(t *testing.T) {
		_, _, exitCode, err := DockerRunner{Image: testImage}.RunCaptureFull(context.Background(), "exit 7", t.TempDir())
		if err != nil {
			t.Fatalf("RunCaptureFull: %v", err)
		}

		if exitCode != 7 {
			t.Errorf("exitCode = %d, want 7", exitCode)
		}
	})

	t.Run("host environment variables are not visible in the container", func(t *testing.T) {
		t.Setenv("STEPS_TEST_HOST_SECRET", "leak-me-not")

		stdout, _, _, err := DockerRunner{Image: testImage}.RunCaptureFull(context.Background(), "echo \"[$STEPS_TEST_HOST_SECRET]\"", t.TempDir())
		if err != nil {
			t.Fatalf("RunCaptureFull: %v", err)
		}

		if stdout != "[]\n" {
			t.Errorf("stdout = %q, want the host env var to be absent inside the container", stdout)
		}
	})

	t.Run("context cancellation terminates the container within the grace window", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		start := time.Now()

		_, _, _, err := DockerRunner{Image: testImage}.RunCaptureFull(ctx, "sleep 30", t.TempDir())

		elapsed := time.Since(start)
		if elapsed > dockerKillGrace+5*time.Second {
			t.Errorf("took %s to terminate after cancellation, want well under the %s grace window", elapsed, dockerKillGrace)
		}

		_ = err // a killed docker client's own exit status varies; only timing is asserted here
	})
}

func TestValidateDockerIntegration(t *testing.T) {
	requireDocker(t)

	err := ValidateDocker(context.Background())
	if err != nil {
		t.Errorf("ValidateDocker: %v", err)
	}
}
