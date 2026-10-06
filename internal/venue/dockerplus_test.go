package venue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

// removeProcessCache drops the cache entries and held outputs this test process left, once every test in it is done: removed per test, they could go from under a parallel test still mounting them.
func removeProcessCache() {
	ctx := context.Background()

	// Containers first, since a volume a container mounts refuses removal: --keep-workspace leaves a session's holder by design.
	//nolint:gosec // a filter built from this process's own pid
	ids, _ := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "label=steps.pid="+strconv.Itoa(os.Getpid())).Output()
	if containers := strings.Fields(string(ids)); len(containers) > 0 {
		_ = exec.CommandContext(ctx, "docker", append([]string{"rm", "-f"}, containers...)...).Run() //nolint:gosec // ids the daemon listed
	}

	//nolint:gosec // a filter built from this process's own pid
	out, _ := exec.CommandContext(ctx, "docker", "volume", "ls", "-q", "--filter", "label=steps.pid="+strconv.Itoa(os.Getpid())).Output()
	// Every volume carrying this pid is this process's: a held get's tree stays in its steps-work- volume, so a prefix list misses one.
	for _, name := range strings.Fields(string(out)) {
		_ = exec.CommandContext(ctx, "docker", "volume", "rm", name).Run() //nolint:gosec // a name the daemon listed
	}
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

	worker := dockerPlusURL(testsshd.New(t), socket)
	cwd := payloadDir(t, 1024)

	runAndClose(t, worker, cwd, "true")

	data := dataOf(t, aliasOf(t, cwd))

	//nolint:gosec // a volume the daemon named
	err := exec.CommandContext(t.Context(), "docker", "run", "--rm", "-v", data+":/d", "alpine:3", "sh", "-c", "echo tampered > /d/blob.bin").Run()
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

	worker := dockerPlusURL(testsshd.New(t), socket)
	hold := func(commands ...string) string { return holdOnce(t, worker, commands...) }

	content := "echo same-" + randomSuffix() + " > out/f"
	first := hold(content)

	// The held volume changed under its alias, as a volume can outside any step.
	alias, err := exec.CommandContext(t.Context(), "docker", "volume", "inspect", "--format", "{{index .Labels \""+cacheData+"\"}}", aliasName(first)).Output() //nolint:gosec // a digest this test was handed
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

	_, err = Pull(t.Context(), shell.RunnerSpec{Worker: worker}, second, t.TempDir())
	if err != nil {
		t.Fatalf("pulling the second step's own output: %v", err)
	}
}

func aliasExists(t *testing.T, digest string) bool {
	t.Helper()

	return exec.CommandContext(t.Context(), "docker", "volume", "inspect", aliasName(digest)).Run() == nil //nolint:gosec // a digest this test was handed
}

// An alias published between commands names a volume the step is still writing, which another build could mount as a lower; HeldOf answers early, the alias waits for close.
func TestDockerPlusPublishesHeldOutputsOnlyAtClose(t *testing.T) {
	socket := hostDockerSocket(t)

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

// A step with nothing cached starts one container, its own: opening its volumes to a non-root user: once cost a busybox chmod per step, a third of every container a worker started. Not parallel, so this process starts nothing else while it counts.
func TestDockerPlusStartsOnlyTheStepsContainer(t *testing.T) {
	requireDockerVenue(t)

	// Streamed, not replayed afterwards: the daemon keeps only its most recent events, and under a full suite this session's had scrolled out of that log by the time it closed. --since covers the moment before the stream connects.
	var started lockedBuffer

	//nolint:gosec // a filter built from this process's own pid and a clock reading
	events := exec.CommandContext(t.Context(), "docker", "events", "--since", eventsTime(time.Now()),
		"--filter", "type=container", "--filter", "event=start", "--filter", "label=steps.pid="+strconv.Itoa(os.Getpid()),
		"--format", "{{.Actor.Attributes.name}}")
	events.Stdout = &started

	err := events.Start()
	if err != nil {
		t.Fatalf("docker events: %v", err)
	}

	t.Cleanup(func() {
		_ = events.Process.Kill()
		_ = events.Wait()
	})

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	err = runner.Run(t.Context(), "true")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	err = runner.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Every helper starts before the step's container, and events arrive in order, so once that one is in, all of them are.
	stepContainer := regexp.MustCompile(`(?m)^steps-[0-9a-f]+$`)
	deadline := time.Now().Add(30 * time.Second)

	for !stepContainer.MatchString(started.String()) {
		if time.Now().After(deadline) {
			t.Fatalf("the step's container never reported starting; saw %q", started.String())
		}

		time.Sleep(50 * time.Millisecond)
	}

	if names := strings.Fields(started.String()); len(names) != 1 {
		t.Errorf("the session started %d containers, want only the step's: %v", len(names), names)
	}
}

// lockedBuffer is a bytes.Buffer a command's output copier writes while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p) //nolint:wrapcheck // a bytes.Buffer never fails
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// eventsTime is docker events' fractional-second form; whole seconds would count a container the previous test started in the same second.
func eventsTime(at time.Time) string {
	return fmt.Sprintf("%d.%09d", at.Unix(), at.Nanosecond())
}

// An agent's tool calls run concurrently; each fetch swaps the same local paths.
func TestDockerPlusConcurrentCommandsFetchSafely(t *testing.T) {
	socket := hostDockerSocket(t)

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

// A container a dead steps process on this machine left on the worker is reclaimed by the next session there.
func TestDockerPlusSweepsAContainerADeadProcessLeft(t *testing.T) {
	socket := hostDockerSocket(t)

	dead := exec.CommandContext(t.Context(), "true")

	err := dead.Run()
	if err != nil {
		t.Fatal(err)
	}

	name := "steps-test-orphan-" + randomSuffix()
	labels := shell.OwnershipLabels()
	args := make([]string, 0, 4+2*len(labels)+3)
	args = append(args, "run", "-d", "--name", name)

	for key, value := range labels {
		if key == "steps.pid" {
			value = strconv.Itoa(dead.Process.Pid)
		}

		args = append(args, "--label", key+"="+value)
	}

	err = exec.CommandContext(t.Context(), "docker", append(args, "alpine:3", "sleep", "60")...).Run() //nolint:gosec // names this test built
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "docker", "rm", "-f", "-v", name).Run() }) //nolint:gosec // as above

	runAndClose(t, dockerPlusURL(testsshd.New(t), socket), t.TempDir(), "true")

	if exec.CommandContext(t.Context(), "docker", "inspect", name).Run() == nil { //nolint:gosec // as above
		t.Fatal("the orphaned container is still on the worker")
	}
}

// A get's in: fills its whole, empty tree; that tree stays on the worker under the artifact's name, as a task's output does, so a consumer there is not sent it back.
func TestDockerPlusHoldsAGetsWholeTree(t *testing.T) {
	socket := hostDockerSocket(t)

	worker := dockerPlusURL(testsshd.New(t), socket)
	cwd := filepath.Join(t.TempDir(), "src")

	err := os.Mkdir(cwd, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	runner, err := NewRunner(shell.RunnerSpec{Image: "alpine:3", Cwd: cwd, FetchAll: true, DeferFetch: true, HoldAs: "src", Worker: worker, WorkerTag: "box"})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	runAll(t, runner, "echo got-"+randomSuffix()+" > blob && mkdir d && echo deep > d/f")

	held, _, ok := HeldOf(runner)
	if !ok || held["src"] == "" {
		t.Fatalf("HeldOf = %v, want the tree held under its artifact name", held)
	}

	if !isEmptyDir(cwd) {
		t.Error("the held tree also came home")
	}

	err = runner.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	dst := t.TempDir()

	_, err = Pull(t.Context(), shell.RunnerSpec{Worker: worker}, held["src"], dst)
	if err != nil {
		t.Fatalf("pulling the held tree: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "d", "f")) //nolint:gosec // a path this test built
	if err != nil || string(got) != "deep\n" {
		t.Fatalf("pulled d/f = %q (%v), want the tree the in: wrote", got, err)
	}
}

// A dropped ssh connection is the tunnel's failure, not the machine's: the next command dials again and finds the step's tree where the volumes kept it, so a retry under attempts: does not fail against a dead session.
func TestDockerPlusRedialsADroppedConnection(t *testing.T) {
	t.Parallel()

	server := testsshd.New(t)
	runner := plusRunnerFor(t, dockerPlusURL(server, hostDockerSocket(t)), t.TempDir())

	err := runner.Run(t.Context(), "echo kept > marker")
	if err != nil {
		t.Fatalf("first command: %v", err)
	}

	plus, ok := runner.(plusRunner)
	if !ok {
		t.Fatalf("runner is %T, want a docker+ runner", runner)
	}

	_ = plus.s.conn.current().Close()

	// Attempt one: may fail on the dropped connection, or already find it gone and dial again.
	_ = runner.Run(t.Context(), "true")

	out, err := runner.RunCapture(t.Context(), "cat marker")
	if err != nil {
		t.Fatalf("a command after the connection dropped: %v", err)
	}

	if string(out) != "kept\n" {
		t.Errorf("marker = %q, want the tree the first command left", out)
	}
}

// Close after a drop with no command since still reaches the daemon: the step's volumes and container would otherwise outlive it on the worker.
func TestDockerPlusReleasesItsVolumesAfterADroppedConnection(t *testing.T) {
	t.Parallel()

	runner := plusRunnerFor(t, dockerPlusURL(testsshd.New(t), hostDockerSocket(t)), t.TempDir())

	err := runner.Run(t.Context(), "true")
	if err != nil {
		t.Fatalf("first command: %v", err)
	}

	made := volumesMadeBy(t, runner)
	for _, name := range made {
		t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "docker", "volume", "rm", "-f", name).Run() }) //nolint:gosec // a name the session made
	}

	dropAndWait(t, &runner.(plusRunner).s.conn)

	err = runner.Close()
	if err != nil {
		t.Fatalf("closing after the connection dropped: %v", err)
	}

	for _, name := range made {
		if volumeExists(t, name) {
			t.Errorf("%s survived its session's close", name)
		}
	}
}

// An output fetch that failed after the command answered leaves the connection suspect, like the command failing would.
func TestDockerPlusSuspectsAConnectionAFetchFailedOn(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()

	err := os.Mkdir(filepath.Join(cwd, "out"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	runner := plusRunnerFor(t, dockerPlusURL(testsshd.New(t), hostDockerSocket(t)), cwd, "out")

	plus, ok := runner.(plusRunner)
	if !ok {
		t.Fatalf("runner is %T, want a docker+ runner", runner)
	}

	err = plus.around(t.Context(), func(shell.Runner) error {
		_ = plus.s.conn.current().Close()

		return nil
	})
	if err == nil {
		t.Fatal("a fetch over a closed connection succeeded")
	}

	if !plus.s.conn.suspect.Load() {
		t.Error("the failed fetch left the connection unsuspected")
	}
}

// The command a drop interrupted keeps running in the container; the retry must not race it in the same tree, so the next command on the new connection finds it gone.
func TestDockerPlusKillsTheCommandADropInterrupted(t *testing.T) {
	t.Parallel()

	runner := plusRunnerFor(t, dockerPlusURL(testsshd.New(t), hostDockerSocket(t)), t.TempDir())
	plus, ok := runner.(plusRunner)
	if !ok {
		t.Fatalf("runner is %T, want a docker+ runner", runner)
	}

	assertInterruptedCommandIsKilled(t, runner, "4321", func() { dropAndWait(t, &plus.s.conn) })
}

// assertInterruptedCommandIsKilled starts a long command, drops the connection under it, and checks the next command finds no trace of it running.
func assertInterruptedCommandIsKilled(t *testing.T, runner shell.Runner, seconds string, drop func()) {
	t.Helper()

	err := runner.Run(t.Context(), "true")
	if err != nil {
		t.Fatalf("first command: %v", err)
	}

	interrupted := make(chan error, 1)

	go func() { interrupted <- runner.Run(t.Context(), "touch started; sleep "+seconds) }()

	deadline := time.Now().Add(20 * time.Second)
	for runner.Run(t.Context(), "test -f started") != nil {
		if time.Now().After(deadline) {
			t.Fatal("the long command never started")
		}

		time.Sleep(100 * time.Millisecond)
	}

	drop()

	err = <-interrupted
	if err == nil {
		t.Fatal("the interrupted command reported success")
	}

	// Attempt one after the drop may fail on the dead connection; the one after runs on the new one.
	_ = runner.Run(t.Context(), "true")

	// The bracket keeps the pattern from matching the sh that runs it.
	out, err := runner.RunCapture(t.Context(), "pgrep -f 'slee[p] "+seconds+"' | wc -l")
	if err != nil {
		t.Fatalf("checking for the interrupted command: %v", err)
	}

	if strings.TrimSpace(string(out)) != "0" {
		t.Errorf("the interrupted command is still running (%s); a retry would race it in the same tree", strings.TrimSpace(string(out)))
	}
}

// A get's tree is held under the artifact name the next step offers it by, which the pipeline says rather than the venue inferring it from a directory's basename.
func TestDockerPlusHoldsAGetsTreeUnderTheNameItWasGiven(t *testing.T) {
	t.Parallel()

	runner, err := NewRunner(shell.RunnerSpec{
		Image: "alpine:3", Cwd: t.TempDir(), Worker: localWorker(t, t.TempDir()).Worker,
		FetchAll: true, DeferFetch: true, HoldAs: "repo",
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	err = runner.Run(t.Context(), "echo fetched > version")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	held, _, ok := HeldOf(runner)
	if !ok || held["repo"] == "" {
		t.Errorf("held = %v, want the tree under %q, the name the next step offers it by", held, "repo")
	}
}
