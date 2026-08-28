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

// TestPlacedContainerizedStepRunsThroughTheRelay is the seam the whole
// containerized-placement design rests on, and the one nothing crossed.
//
// The two halves have always been tested apart: the relay carries bytes to
// the worker's daemon, and internal/shell knows how to run a step in a
// container. What was never carried ACROSS is the client — and the client is
// exactly what changed when container execution stopped spawning `docker`.
// The sibling test above still proves the transport with the CLI, which is no
// longer the thing production uses; this one drives the same socket with the
// engine client a real placed step drives it with.
//
// The difference is not cosmetic. A CLI opens a connection per invocation and
// closes it; a library pools them and keeps them alive between calls, and the
// relay hands its wire back to the session between commands. A step fetching
// its outputs over that same wire is what breaks if an idle connection is not
// as quiet as it looks.
//
// What this CANNOT prove is which daemon answered: a local: worker is this
// machine, so both ends are one daemon and a container started on the wrong
// one looks identical. That half is TestWorkerSweepTargetsTheWorkerDaemon's.
// What it does prove is that the traffic crosses the wire at all — remove the
// routing bracket and this hangs, because nothing is left to carry the
// daemon's answers back.
func TestPlacedContainerizedStepRunsThroughTheRelay(t *testing.T) {
	requireDockerVenue(t)

	spec := localWorker(t, t.TempDir())
	spec.Image = "alpine:3"

	placed := newLocalRunner(t, spec)

	// Reads a file that exists in the IMAGE and on no developer machine this
	// runs on, so a step that quietly executed on the host instead of in a
	// container fails here rather than passing with an echo either could
	// produce.
	stdout, stderr, code, err := placed.RunCaptureFull(context.Background(), "cat /etc/alpine-release")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v (stderr %q)", err, stderr)
	}

	if code != 0 || strings.TrimSpace(stdout) == "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want the command to have run in the image on the worker",
			code, stdout, stderr)
	}

	// A second command, because one proves the connection was made and two
	// prove it survived the wire being handed back. This is where a pooled
	// connection left open across the handoff would show up.
	stdout, stderr, code, err = placed.RunCaptureFull(context.Background(), "echo again")
	if err != nil {
		t.Fatalf("second RunCaptureFull: %v (stderr %q)", err, stderr)
	}

	if code != 0 || strings.TrimSpace(stdout) != "again" {
		t.Errorf("exit %d, stdout %q, stderr %q; want a second command through the same session",
			code, stdout, stderr)
	}

	// And the session still owns its wire for everything that is not docker.
	// A step fetches its outputs this way the moment its command is done, so
	// a relay that did not give the connection back loses the work rather
	// than reporting anything.
	err = placed.Close()
	if err != nil {
		t.Errorf("Close: %v; the session did not recover its wire after the container", err)
	}
}
