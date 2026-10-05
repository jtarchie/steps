package venue

// The worker's docker daemon, reached by this machine's own docker client.

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// requireDockerVenue mirrors internal/shell's requireDocker; this package
// cannot reach that one, and six lines beat exporting a test helper.
func requireDockerVenue(t *testing.T) {
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
