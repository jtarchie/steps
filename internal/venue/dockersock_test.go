package venue

// The worker's docker daemon, reached by this machine's own docker client.

import (
	"context"
	"os"
	"os/exec"
	"strings"
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

// TestDockerSocketReachesTheWorkersDaemon is the venue half of the transport:
// a unix socket on THIS machine that the ordinary docker client can drive,
// carried to the worker's daemon over the session's own connection.
//
// Proven with the real client rather than a hand-written request, because
// what has to work is precisely that an unmodified `docker` talks to it: the
// whole design is that internal/shell's container code retargets by
// environment variable and changes not a line.
func TestDockerSocketReachesTheWorkersDaemon(t *testing.T) {
	requireDockerVenue(t)

	placed := newLocalRunner(t, localWorker(t, t.TempDir()))

	// Forces the handshake, so the session is live and idle.
	err := placed.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	remote, ok := placed.(runner)
	if !ok {
		t.Fatalf("runner is %T, not a placed one", placed)
	}

	socket, stop, err := remote.session.serveDockerSocket(context.Background())
	if err != nil {
		t.Fatalf("serveDockerSocket: %v", err)
	}

	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	cmd.Env = append(os.Environ(), "DOCKER_HOST=unix://"+socket)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker version through the forwarded socket: %v\n%s", err, out)
	}

	if strings.TrimSpace(string(out)) == "" {
		t.Errorf("the daemon answered nothing: %q", out)
	}

	t.Logf("worker daemon version through the wire: %s", strings.TrimSpace(string(out)))

	// The point of the handoff: the router owned the connection while the
	// socket was up, and the session has to get it back — a step fetches its
	// outputs over this same wire the moment the command is done.
	stop()

	err = placed.Run(context.Background(), "echo after-docker")
	if err != nil {
		t.Fatalf("the session did not recover its wire after the docker socket closed: %v", err)
	}
}
