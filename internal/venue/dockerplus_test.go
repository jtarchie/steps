package venue

import (
	"context"
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

// sessionVolumes counts this process's non-cache volumes: what a closed session must not leave.
func sessionVolumes(t *testing.T) int {
	t.Helper()

	//nolint:gosec // a filter this test built from its own pid
	out, err := exec.CommandContext(t.Context(), "docker", "volume", "ls", "--format", "{{.Name}} {{.Labels}}", "--filter", "label=steps.pid="+strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Fatalf("listing volumes: %v", err)
	}

	count := 0

	for line := range strings.Lines(string(out)) {
		if !strings.Contains(line, cacheLabel+"=") {
			count++
		}
	}

	return count
}

func cleanCache(t *testing.T) {
	t.Helper()

	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())

		//nolint:gosec // a filter built from this process's own pid
		out, _ := exec.CommandContext(ctx, "docker", "volume", "ls", "-q", "--filter", "label=steps.pid="+strconv.Itoa(os.Getpid())).Output()
		for _, name := range strings.Fields(string(out)) {
			// The cache, and the outputs it names: everything a closed session leaves on purpose.
			if strings.HasPrefix(name, aliasPrefix) || strings.HasPrefix(name, "steps-d-") || strings.HasPrefix(name, "steps-out-") {
				_ = exec.CommandContext(ctx, "docker", "volume", "rm", name).Run() //nolint:gosec // a name the daemon listed
			}
		}
	})
}

func payloadDir(t *testing.T, size int) string {
	t.Helper()

	cwd := t.TempDir()

	err := os.Mkdir(filepath.Join(cwd, "src"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(cwd, "src", "blob.bin"), strings.Repeat(randomSuffix(), size/16))

	return cwd
}

func runAndClose(t *testing.T, worker, cwd, command string) Placement {
	t.Helper()

	runner, err := NewRunner(shell.RunnerSpec{Image: "alpine:3", Cwd: cwd, Worker: worker, WorkerTag: "box"})
	if err != nil {
		t.Fatal(err)
	}

	// Also a cleanup: a Fatal below skips the explicit Close, and an open session holds the test sshd's cleanup forever.
	t.Cleanup(func() { _ = runner.Close() })

	err = runner.Run(t.Context(), command)
	if err != nil {
		t.Fatalf("Run(%q): %v", command, err)
	}

	placement, _ := PlacementOf(runner)

	err = runner.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	return placement
}

// The per-artifact cache: an input the worker has already seen is offered by digest and not sent again.
func TestDockerPlusSendsAnUnchangedInputOnce(t *testing.T) {
	socket := hostDockerSocket(t)
	cleanCache(t)

	worker := dockerPlusURL(testsshd.New(t), socket)
	cwd := payloadDir(t, 1<<20)

	first := runAndClose(t, worker, cwd, "test -s src/blob.bin")
	second := runAndClose(t, worker, cwd, "test -s src/blob.bin")

	if first.BytesSent < 1<<20 {
		t.Fatalf("the first run sent %d bytes, want the whole input", first.BytesSent)
	}

	if second.BytesSent >= 1<<20 {
		t.Fatalf("the second run sent %d bytes for an input the worker already holds", second.BytesSent)
	}
}

// Copy-on-write: a step that writes into its input changes its own view, never the cached tree the next step is handed.
func TestDockerPlusStepWritesNeverReachTheCache(t *testing.T) {
	socket := hostDockerSocket(t)
	cleanCache(t)

	before := sessionVolumes(t)
	worker := dockerPlusURL(testsshd.New(t), socket)
	cwd := payloadDir(t, 1024)

	runAndClose(t, worker, cwd, "echo vandal > src/blob.bin && touch src/extra")
	second := runAndClose(t, worker, cwd, "test ! -e src/extra && ! grep -q vandal src/blob.bin")

	if second.BytesSent >= 1024 {
		t.Fatalf("the second run re-sent the input (%d bytes): the cache was not reused", second.BytesSent)
	}

	if after := sessionVolumes(t); after != before {
		t.Fatalf("closed sessions left %d volumes beyond the cache", after-before)
	}
}

// A cached tree is re-hashed before reuse: one changed under the alias is a miss and is sent again, never handed to a step as the tree it asked for.
func TestDockerPlusRefusesATamperedCacheEntry(t *testing.T) {
	socket := hostDockerSocket(t)
	cleanCache(t)

	worker := dockerPlusURL(testsshd.New(t), socket)
	cwd := payloadDir(t, 1024)

	runAndClose(t, worker, cwd, "true")

	//nolint:gosec // a filter built from this process's own pid
	out, err := exec.CommandContext(t.Context(), "docker", "volume", "ls", "-q", "--filter", "label="+cacheLabel+"=data", "--filter", "label=steps.pid="+strconv.Itoa(os.Getpid())).Output()
	if err != nil || len(strings.Fields(string(out))) != 1 {
		t.Fatalf("want one data volume, got %q (%v)", out, err)
	}

	data := strings.TrimSpace(string(out))

	//nolint:gosec // a volume the daemon named
	err = exec.CommandContext(t.Context(), "docker", "run", "--rm", "-v", data+":/d", "alpine:3", "sh", "-c", "echo tampered > /d/blob.bin").Run()
	if err != nil {
		t.Fatalf("tampering: %v", err)
	}

	second := runAndClose(t, worker, cwd, "! grep -q tampered src/blob.bin")
	if second.BytesSent < 1024 {
		t.Fatalf("the tampered entry was reused (%d bytes sent)", second.BytesSent)
	}
}

// An alias an earlier command filed names a volume its step went on writing to; a later step whose output hashes to that old digest must keep its own tree, not be handed the changed one.
func TestDockerPlusHoldsPastAStaleAlias(t *testing.T) {
	socket := hostDockerSocket(t)
	cleanCache(t)

	worker := dockerPlusURL(testsshd.New(t), socket)
	hold := func(commands ...string) string { return holdOnce(t, worker, commands...) }

	content := "echo same-" + randomSuffix() + " > out/f"
	first := hold(content)

	// The held volume changed under its alias, as a volume can outside any step.
	alias, err := exec.CommandContext(t.Context(), "docker", "volume", "inspect", "--format", "{{index .Labels \""+cacheData+"\"}}", aliasPrefix+first).Output() //nolint:gosec // a digest this test was handed
	if err != nil {
		t.Fatal(err)
	}

	//nolint:gosec // a volume the daemon named
	err = exec.CommandContext(t.Context(), "docker", "run", "--rm", "-v", strings.TrimSpace(string(alias))+":/d", "alpine:3", "sh", "-c", "echo tampered > /d/f").Run()
	if err != nil {
		t.Fatalf("tampering: %v", err)
	}

	second := hold(content)
	if second != first {
		t.Fatalf("the same output digested %s then %s", first, second)
	}

	_, err = Pull(t.Context(), shell.RunnerSpec{Worker: worker}, "out", second, t.TempDir())
	if err != nil {
		t.Fatalf("pulling the second step's own output: %v", err)
	}
}

func aliasExists(t *testing.T, digest string) bool {
	t.Helper()

	return exec.CommandContext(t.Context(), "docker", "volume", "inspect", aliasPrefix+digest).Run() == nil //nolint:gosec // a digest this test was handed
}

// An alias published between commands names a volume the step is still writing, which another build could mount as a lower; HeldOf answers early, the alias waits for close.
func TestDockerPlusPublishesHeldOutputsOnlyAtClose(t *testing.T) {
	socket := hostDockerSocket(t)
	cleanCache(t)

	runner := deferredRunner(t, dockerPlusURL(testsshd.New(t), socket))
	runAll(t, runner, "echo first-"+randomSuffix()+" > out/f")

	first, _, _ := HeldOf(runner)

	runAll(t, runner, "echo second-"+randomSuffix()+" > out/f")

	final, _, ok := HeldOf(runner)
	if !ok || final["out"] == "" || final["out"] == first["out"] {
		t.Fatalf("HeldOf after each command: %v then %v", first, final)
	}

	if aliasExists(t, first["out"]) || aliasExists(t, final["out"]) {
		t.Fatal("an alias was published while the step could still write its volume")
	}

	err := runner.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !aliasExists(t, final["out"]) {
		t.Fatal("the final output was not held after close")
	}

	if aliasExists(t, first["out"]) {
		t.Fatal("an intermediate digest was held: it names a tree that no longer exists")
	}
}

// A non-root user: must still write its outputs and the top of its tree; fresh volume roots are created root-owned.
func TestDockerPlusNonRootUserWritesItsOutputs(t *testing.T) {
	socket := hostDockerSocket(t)
	cleanCache(t)

	cwd := payloadDir(t, 1024)

	// 0644, as a checkout's files are: inputs reach the worker root-owned with the modes they had, so only a world-readable file is readable to a non-root user:.
	err := os.Chmod(filepath.Join(cwd, "src", "blob.bin"), 0o644) //nolint:gosec // the point of the test
	if err != nil {
		t.Fatal(err)
	}

	err = os.Mkdir(filepath.Join(cwd, "out"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	runner, err := NewRunner(shell.RunnerSpec{Image: "alpine:3", User: "1000:1000", Cwd: cwd, Fetch: []string{"out"}, Worker: dockerPlusURL(testsshd.New(t), socket), WorkerTag: "box"})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	err = runner.Run(t.Context(), "id -u > out/uid && cat src/blob.bin > /dev/null && touch scratch && touch src/new")
	if err != nil {
		t.Fatalf("Run as 1000: %v", err)
	}

	if got := mustRead(t, filepath.Join(cwd, "out", "uid")); got != "1000\n" {
		t.Fatalf("out/uid = %q", got)
	}
}

// An agent's tool calls run concurrently; each fetch swaps the same local paths.
func TestDockerPlusConcurrentCommandsFetchSafely(t *testing.T) {
	socket := hostDockerSocket(t)
	cleanCache(t)

	cwd := t.TempDir()

	err := os.Mkdir(filepath.Join(cwd, "out"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	runner := plusRunnerFor(t, dockerPlusURL(testsshd.New(t), socket), cwd, "out")

	err = runner.Run(t.Context(), "echo x > out/f")
	if err != nil {
		t.Fatal(err)
	}

	// The fetches themselves, sixteen at once: a command takes tens of milliseconds and a fetch a few, so racing whole commands rarely overlaps the part that swaps paths.
	session := runner.(plusRunner).s

	errs := make(chan error, 16)
	for range 16 {
		go func() { errs <- session.fetch(t.Context()) }()
	}

	for range 16 {
		err := <-errs
		if err != nil {
			t.Errorf("a concurrent command: %v", err)
		}
	}
}

// deferredRunner is a step with one empty output, held on the worker rather than fetched.
func deferredRunner(t *testing.T, worker string) shell.Runner {
	t.Helper()

	cwd := t.TempDir()

	err := os.Mkdir(filepath.Join(cwd, "out"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	runner, err := NewRunner(shell.RunnerSpec{Image: "alpine:3", Cwd: cwd, Fetch: []string{"out"}, DeferFetch: true, Worker: worker, WorkerTag: "box"})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	return runner
}

func runAll(t *testing.T, runner shell.Runner, commands ...string) {
	t.Helper()

	for _, command := range commands {
		err := runner.Run(t.Context(), command)
		if err != nil {
			t.Fatalf("Run(%q): %v", command, err)
		}
	}
}

// holdOnce runs a deferred step to its close and returns the digest its output was held under.
func holdOnce(t *testing.T, worker string, commands ...string) string {
	t.Helper()

	runner := deferredRunner(t, worker)
	runAll(t, runner, commands...)

	held, _, _ := HeldOf(runner)

	err := runner.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	return held["out"]
}
