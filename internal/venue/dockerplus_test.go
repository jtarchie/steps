package venue

import (
	"errors"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/testsshd"
)

func dockerPlusURL(server *testsshd.Server, socket string) string {
	return "docker+ssh://" + server.Addr() + "?" + url.Values{
		"identity":    {server.Identity},
		"known_hosts": {server.KnownHosts},
		"ssh_config":  {"none"},
		"sock":        {socket},
	}.Encode()
}

func hostDockerSocket(t *testing.T) string {
	t.Helper()

	requireDockerVenue(t)

	host, err := dockerapi.ResolveHost()
	if err != nil {
		t.Skipf("resolving the docker host: %v", err)
	}

	socket, ok := strings.CutPrefix(host, "unix://")
	if !ok {
		t.Skipf("docker endpoint %q is not a unix socket", host)
	}

	return socket
}

// pidVolumes counts this process's volumes, the only ones a leak here could be.
func pidVolumes(t *testing.T) int {
	t.Helper()

	//nolint:gosec // a filter this test built from its own pid
	out, err := exec.CommandContext(t.Context(), "docker", "volume", "ls", "-q", "--filter", "label=steps.pid="+strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Fatalf("listing volumes: %v", err)
	}

	return len(strings.Fields(string(out)))
}

func plusRunnerFor(t *testing.T, worker, cwd string, fetch ...string) shell.Runner {
	t.Helper()

	runner, err := NewRunner(shell.RunnerSpec{Image: "alpine:3", Cwd: cwd, Fetch: fetch, Worker: worker, WorkerTag: "box"})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	return runner
}

// A live sshd in front of a socket nothing listens on must fail naming the socket, before any volume exists on the daemon.
func TestDockerPlusNamesADeadDaemonBeforeMovingAFile(t *testing.T) {
	hostDockerSocket(t)

	before := pidVolumes(t)
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, "input.txt"), "x")

	missing := filepath.Join(t.TempDir(), "no-daemon.sock")
	err := plusRunnerFor(t, dockerPlusURL(testsshd.New(t), missing), cwd).Run(t.Context(), "true")

	if err == nil || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("want an error naming %s, got %v", missing, err)
	}

	if after := pidVolumes(t); after != before {
		t.Fatalf("%d volumes were created for a daemon that never answered", after-before)
	}
}

func TestDockerPlusNamesADeadSSHD(t *testing.T) {
	t.Parallel()

	server := testsshd.New(t)
	worker := dockerPlusURL(server, "/var/run/docker.sock")

	// Taken and released: nothing listens there now.
	worker = strings.Replace(worker, server.Addr(), deadAddress(t), 1)

	err := plusRunnerFor(t, worker, t.TempDir()).Run(t.Context(), "true")
	if err == nil || !errors.Is(err, ErrWorker) || !strings.Contains(err.Error(), "docker+ssh://") {
		t.Fatalf("want a worker error naming the mapping, got %v", err)
	}
}

func TestDockerPlusRefusesAStepWithNoImage(t *testing.T) {
	t.Parallel()

	_, err := NewRunner(shell.RunnerSpec{Cwd: t.TempDir(), Worker: "docker+ssh://box", WorkerTag: "box"})
	if !errors.Is(err, errNoImageOnDocker) {
		t.Fatalf("want errNoImageOnDocker, got %v", err)
	}
}

// The local tree must reflect the worker after a FAILED command too: an assert: or a fix: agent reads what the step left.
func TestDockerPlusFetchesOutputsAfterANonzeroExit(t *testing.T) {
	socket := hostDockerSocket(t)
	before := pidVolumes(t)

	cwd := t.TempDir()

	err := os.Mkdir(filepath.Join(cwd, "out"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	runner := plusRunnerFor(t, dockerPlusURL(testsshd.New(t), socket), cwd, "out")

	_, _, code, err := runner.RunCaptureFull(t.Context(), "echo partial > out/log.txt; exit 3")
	if err != nil || code != 3 {
		t.Fatalf("RunCaptureFull: code %d, %v; want exit 3 as data", code, err)
	}

	if got := mustRead(t, filepath.Join(cwd, "out", "log.txt")); got != "partial\n" {
		t.Fatalf("out/log.txt = %q after the failed command", got)
	}

	// Run reports the exit as an error rather than as data, which is the branch that must still fetch.
	err = runner.Run(t.Context(), "echo again > out/run.txt; exit 4")
	if !shell.IsExitError(err) {
		t.Fatalf("Run: %v, want the exit as an ExitError", err)
	}

	if got := mustRead(t, filepath.Join(cwd, "out", "run.txt")); got != "again\n" {
		t.Fatalf("out/run.txt = %q after the failed Run", got)
	}

	err = runner.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	if after := pidVolumes(t); after != before {
		t.Fatalf("Close left %d volumes on the daemon", after-before)
	}
}

// deadAddress is a loopback port that was just listening and no longer is.
func deadAddress(t *testing.T) string {
	t.Helper()

	var listenConfig net.ListenConfig

	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	address := listener.Addr().String()
	_ = listener.Close()

	return address
}
