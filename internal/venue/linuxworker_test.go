package venue

// A ROOT shim on Linux, driven from this machine.
//
// The one worker shape nothing here had ever actually run. Every other test
// drives a worker that is this machine — local: is a child process, and the
// in-process sshd logs into the account running the tests — so the shim's
// identity and platform always matched the orchestrator's, and every answer
// derived from the WORKER was indistinguishable from the same answer derived
// from here. That is precisely the class of bug the venue keeps producing:
// the mount path, the container user, and the docker host were each computed
// from the wrong machine at some point, and each was invisible until a worker
// that genuinely differed ran one.
//
// An alpine container with sshd is the cheapest real difference available: a
// different operating system from a darwin orchestrator, a different
// architecture from some of them, and uid 0 against an orchestrator that is
// never uid 0.

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/shell"
)

// linuxWorker is a running container reachable as root over ssh.
type linuxWorker struct {
	url      string
	identity string
	binary   string
	// container is the worker itself, for a fixture that has to reach past
	// ssh — seeding the worker's own daemon with an image, for one.
	container string
}

// startLinuxWorker builds the image, generates a keypair, runs the container
// and cross-compiles the shim it will be sent.
func startLinuxWorker(t *testing.T) linuxWorker {
	t.Helper()

	return startLinuxWorkerWith(t, linuxWorkerDockerfile)
}

// workerScratchRoot is where a worker keeps its trees.
const workerScratchRoot = "/var/tmp/steps"

// startLinuxWorkerWithDocker is startLinuxWorker with a daemon OF ITS OWN,
// which is what a containerized placed step needs.
//
// Its own, rather than this machine's socket handed in. That costs a
// docker-in-docker image and a privileged container, and it buys the only
// thing worth buying here: with one shared daemon the test cannot tell a
// container started through the forwarded socket from one started on the
// orchestrator — both resolve the same paths against the same filesystem, so
// aiming at the wrong daemon passes. A worker whose daemon knows nothing about
// this machine cannot be satisfied by accident.
//
// The image is loaded into that daemon over a pipe rather than pulled, so the
// test needs no more network than the fixture already does — the plain worker
// image runs `apk add` at build time, so this file has never been offline.
func startLinuxWorkerWithDocker(t *testing.T, seedImage bool) linuxWorker {
	t.Helper()

	worker := startLinuxWorkerWith(t, dindWorkerDockerfile, "--privileged")

	waitForWorkerDaemon(t, worker.container)

	if seedImage {
		loadImageIntoWorker(t, worker.container, containerizedStepImage)
	}

	return worker
}

// containerizedStepImage is the image the placed step runs in. Small, and
// already on this machine because every other docker test here uses it.
const containerizedStepImage = "alpine:3"

// waitForWorkerDaemon blocks until the worker's own dockerd is answering.
func waitForWorkerDaemon(t *testing.T, container string) {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)

	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

		//nolint:gosec // container is an id this test just minted
		err := exec.CommandContext(ctx, "docker", "exec", container, "docker", "info").Run()

		cancel()

		if err == nil {
			return
		}

		time.Sleep(250 * time.Millisecond)
	}

	t.Fatal("the worker's own docker daemon never came up")
}

// loadImageIntoWorker streams an image from this machine's daemon into the
// worker's, so the worker never reaches a registry.
func loadImageIntoWorker(t *testing.T, container, image string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	//nolint:gosec // image is a constant in this file
	save := exec.CommandContext(ctx, "docker", "save", image)

	stream, err := save.StdoutPipe()
	if err != nil {
		t.Fatalf("saving %s: %v", image, err)
	}

	//nolint:gosec // container is an id this test just minted
	load := exec.CommandContext(ctx, "docker", "exec", "-i", container, "docker", "load")
	load.Stdin = stream

	err = save.Start()
	if err != nil {
		t.Fatalf("saving %s: %v", image, err)
	}

	out, err := load.CombinedOutput()
	if err != nil {
		t.Fatalf("loading %s into the worker: %v\n%s", image, err, out)
	}

	err = save.Wait()
	if err != nil {
		t.Fatalf("saving %s: %v", image, err)
	}
}

// startLinuxWorkerWith is the fixture, over a given image and run arguments.
func startLinuxWorkerWith(t *testing.T, dockerfile string, extraRunArgs ...string) linuxWorker {
	t.Helper()

	requireDockerVenue(t)

	_, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not found on PATH")
	}

	dir := t.TempDir()
	identity := filepath.Join(dir, "id_ed25519")

	run(t, dir, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", identity)

	writeFile(t, filepath.Join(dir, "Dockerfile"), dockerfile)
	copyFile(t, identity+".pub", filepath.Join(dir, "authorized_keys"))

	// A tag per test run, so a stale image from an older Dockerfile cannot be
	// reused silently.
	image := "steps-linux-worker:" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))

	run(t, dir, "docker", "build", "-q", "-t", image, ".")

	runArgs := append([]string{"run", "-d", "-P"}, extraRunArgs...)
	runArgs = append(runArgs, image)

	id := strings.TrimSpace(run(t, dir, "docker", runArgs...))

	t.Cleanup(func() {
		// Its own context, and a bound: cleanup most often runs because the
		// test's context was just cancelled, and a give-back that inherits
		// that cancellation leaves the container it was removing.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 30*time.Second)
		defer cancel()

		//nolint:gosec // id and image are strings this test just minted
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", id).Run()
		//nolint:gosec // as above
		_ = exec.CommandContext(ctx, "docker", "rmi", "-f", image).Run()
	})

	port := hostPort(t, dir, id)

	binary := buildLinuxShim(t, dir)

	// Encoded, never concatenated: a SHA256 fingerprint is base64 and a "+" in
	// a raw query decodes to a SPACE, so the pin arrived mangled and the
	// worker was refused for a typo nobody made.
	query := url.Values{
		"identity":   {identity},
		"hostkey":    {hostKeyFingerprint(t, dir, port)},
		"ssh_config": {"none"},
		"binary":     {binary},
	}

	return linuxWorker{
		url:       "ssh://root@127.0.0.1:" + port + workerScratchRoot + "?" + query.Encode(),
		identity:  identity,
		binary:    binary,
		container: id,
	}
}

// linuxWorkerDockerfile is the whole worker: sshd, root, a key, and nothing
// else. No steps binary is baked in — the venue pushes one, which is the
// contract being exercised.
//
// ONE host key, ed25519, rather than the set ssh-keygen -A makes. ?hostkey=
// pins a single fingerprint, so a server offering several leaves which one is
// pinned up to whichever algorithm the client and server negotiate — and the
// pin then fails against a key that is perfectly legitimate.
const linuxWorkerDockerfile = `FROM alpine:3.20
RUN apk add --no-cache openssh-server && \
    ssh-keygen -q -t ed25519 -N '' -f /etc/ssh/ssh_host_ed25519_key && \
    mkdir -p /root/.ssh && chmod 700 /root/.ssh
COPY authorized_keys /root/.ssh/authorized_keys
RUN chmod 600 /root/.ssh/authorized_keys && \
    printf 'PermitRootLogin prohibit-password\nPubkeyAuthentication yes\nSubsystem sftp internal-sftp\nHostKey /etc/ssh/ssh_host_ed25519_key\n' >> /etc/ssh/sshd_config
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
`

// dindWorkerDockerfile is the same worker with a docker daemon of its own.
//
// openssh-client-default is upgraded alongside the server because the dind
// image ships a newer client than the alpine index offers, and apk refuses the
// pair rather than choosing — a build failure that says nothing about ssh.
//
// TLS is switched off and the daemon is pinned to the unix socket, because the
// socket is the whole interface here: the shim dials it directly and the venue
// forwards the bytes. Nothing ever speaks to this daemon over the network.
//
// sshd is started in the background and the dind entrypoint keeps the
// container alive, so the two survive together — a container whose PID 1 is
// sshd would have no daemon, and one whose PID 1 is dockerd would have no way
// in.
const dindWorkerDockerfile = `FROM docker:27-dind
RUN apk add --no-cache --upgrade openssh-server openssh-client-default && \
    ssh-keygen -q -t ed25519 -N '' -f /etc/ssh/ssh_host_ed25519_key && \
    mkdir -p /root/.ssh && chmod 700 /root/.ssh
COPY authorized_keys /root/.ssh/authorized_keys
RUN chmod 600 /root/.ssh/authorized_keys && \
    printf 'PermitRootLogin prohibit-password\nPubkeyAuthentication yes\nSubsystem sftp internal-sftp\nHostKey /etc/ssh/ssh_host_ed25519_key\n' >> /etc/ssh/sshd_config
ENV DOCKER_TLS_CERTDIR=""
EXPOSE 22
CMD ["sh", "-c", "/usr/sbin/sshd -e && exec dockerd-entrypoint.sh dockerd --host=unix:///var/run/docker.sock"]
`

// buildLinuxShim cross-compiles the steps binary the worker will run.
//
// steps has no Go toolchain in the field, which is why ?binary= exists at all;
// here the toolchain is the one running the tests. CGO_ENABLED=0 is what makes
// the result pushable, and is the same guard `task build` keeps.
func buildLinuxShim(t *testing.T, dir string) string {
	t.Helper()

	binary := filepath.Join(dir, "steps-linux")

	//nolint:gosec // binary is a path under this test's own TempDir
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	// The repo root, where the main package is: this file is two levels down.
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cross-compiling the shim for linux/%s: %v\n%s", runtime.GOARCH, err, out)
	}

	return binary
}

// hostPort is where the container's sshd is published.
func hostPort(t *testing.T, dir, id string) string {
	t.Helper()

	// Published ports are not necessarily readable the instant `docker run`
	// returns, and neither is sshd listening behind one.
	deadline := time.Now().Add(30 * time.Second)

	for {
		// Soft: a published port is not readable the instant `docker run`
		// returns, so a failure here is "not yet" rather than "not ever".
		mapping := strings.TrimSpace(softRun(dir, "docker", "port", id, "22/tcp"))
		if mapping != "" {
			port := mapping[strings.LastIndex(mapping, ":")+1:]

			if sshAnswers(dir, port) {
				return port
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("sshd in %s never answered", id)
		}

		time.Sleep(250 * time.Millisecond)
	}
}

func sshAnswers(dir, port string) bool {
	//nolint:gosec // port is a number this test read back from docker
	cmd := exec.CommandContext(context.Background(), "ssh-keyscan", "-p", port, "127.0.0.1")
	cmd.Dir = dir

	out, err := cmd.Output()

	return err == nil && len(out) > 0
}

// hostKeyFingerprint pins the container's host key the way an acquired machine
// is pinned: by fingerprint, since it has no known_hosts entry and never will.
func hostKeyFingerprint(t *testing.T, dir, port string) string {
	t.Helper()

	scanned := filepath.Join(dir, "scanned")
	writeFile(t, scanned, runIn(t, dir, "ssh-keyscan", "-t", "ed25519", "-p", port, "127.0.0.1"))

	// "256 SHA256:... 127.0.0.1 (ED25519)" — the fingerprint is the second
	// field, spelled exactly as the worker URL wants it.
	fields := strings.Fields(run(t, dir, "ssh-keygen", "-lf", scanned))
	if len(fields) < 2 {
		t.Fatalf("could not read a fingerprint from %s", scanned)
	}

	return fields[1]
}

// softRun is run for a command that is expected to fail while something is
// still starting: it answers empty rather than failing the test.
func softRun(dir string, name string, args ...string) string {
	//nolint:gosec // every caller passes a constant program and this test's own paths
	cmd := exec.CommandContext(context.Background(), name, args...)
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	return string(out)
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()

	return runIn(t, dir, name, args...)
}

func runIn(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()

	//nolint:gosec // every caller passes a constant program and this test's own paths
	cmd := exec.CommandContext(t.Context(), name, args...)
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v", name, strings.Join(args, " "), err)
	}

	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	err := os.WriteFile(path, []byte(content), 0o600) //nolint:gosec // every caller passes a path under this test's own TempDir
	if err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()

	data, err := os.ReadFile(from) //nolint:gosec // a path this test just made
	if err != nil {
		t.Fatalf("reading %s: %v", from, err)
	}

	writeFile(t, to, string(data))
}

// TestLinuxRootWorkerReportsItsOwnIdentity is the gap stated as an assertion:
// the facts a placed step is decided from come from the WORKER.
func TestLinuxRootWorkerReportsItsOwnIdentity(t *testing.T) {
	t.Parallel()

	worker := startLinuxWorker(t)

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner, err := NewRunner(shell.RunnerSpec{
		Cwd:       cwd,
		Worker:    worker.url,
		WorkerTag: "linux",
		Fetch:     []string{"out"},
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	err = runner.Run(context.Background(), `cat data/seed.txt > out/report.txt; id -u >> out/report.txt`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	report := mustRead(t, filepath.Join(cwd, "out", "report.txt"))
	if report != "seed\n0\n" {
		t.Errorf("report = %q, want the tree round-tripped and the step running as root", report)
	}

	placement, ok := PlacementOf(runner)
	if !ok {
		t.Fatal("a finished placed step described no machine")
	}

	if placement.GOOS != "linux" {
		t.Errorf("goos = %q, want the WORKER's linux and not this machine's %s", placement.GOOS, runtime.GOOS)
	}

	if placement.UID == nil || *placement.UID != 0 {
		t.Errorf("uid = %v, want a reported 0 — the shim runs as root here and every other worker in this package does not", placement.UID)
	}
}

// TestLinuxRootWorkerDecidesTheContainerUser is the seam the unit test could
// only approximate: a REAL hello from a root Linux shim, feeding the decision
// that a container on that worker writes as root.
//
// Computed from this machine instead, it produced --user 501:20 on darwin or
// --user 1000:1000 from a Linux orchestrator — over a root-owned tree the
// container then could not write, and in the second case could not even read.
func TestLinuxRootWorkerDecidesTheContainerUser(t *testing.T) {
	t.Parallel()

	worker := startLinuxWorker(t)

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")

	// No Image on the spec: what is being asserted is the DECISION, not the
	// daemon — the same facts, from the same live hello, through the same
	// function the containerized path calls, without paying for a worker that
	// can actually run a container. TestLinuxRootWorkerRunsAContainerizedStep
	// pays for one and runs the whole thing.
	built, err := NewRunner(shell.RunnerSpec{
		Cwd:    cwd,
		Worker: worker.url,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = built.Close() })

	// The handshake has to have happened: a session dials lazily, so nothing
	// is known about the worker until it has been asked to do something.
	err = built.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	placed, ok := built.(runner)
	if !ok {
		t.Fatalf("expected a venue runner, got %T", built)
	}

	placed.session.container.Image = "alpine"

	spec := placed.session.containerSpec("unix:///tmp/x.sock")

	if spec.User != "0:0" {
		t.Errorf("container user = %q, want 0:0 from the worker's own hello", spec.User)
	}

	// And the tree it would mount is the one on the WORKER, which is the other
	// answer this machine cannot give.
	if !strings.HasPrefix(spec.MountPath, "/var/tmp/steps/") {
		t.Errorf("mount = %q, want the worker's tree under the root its URL named", spec.MountPath)
	}
}

// TestLinuxRootWorkerRunsAContainerizedStep is the seam the venue's whole
// containerized-placement design rests on, and the one nothing crossed.
//
// Every piece has been tested apart. ssh_test.go round-trips a step over a
// real transport. dockersock_test.go carries an engine client's traffic to a
// worker's daemon. The tests above prove a worker that genuinely differs from
// this machine answers for its own identity and its own tree. What no test
// ever did was run ONE step through all of them at once — a real ssh worker,
// with an image — and the reason is stated in this file's own comment further
// up: an alpine box has no docker daemon, so the combination was skipped
// rather than covered.
//
// It matters because that intersection is where this package's worst bugs
// have lived. The mount path, the container user, and the docker host were
// each computed from the wrong machine at some point, and each was invisible
// until something ran on a worker that was not this one. A containerized
// placed step is the only shape that exercises all three at once.
//
// The worker has a daemon of its own, which is what makes the crossing real:
// it knows nothing about this machine, so a container started on the WRONG
// daemon cannot find the tree and cannot pass.
//
// Two assertions, and both had to be chosen carefully.
//
// "Did it run in a container" cannot be answered by anything alpine-shaped:
// the WORKER is an alpine box, so /etc/alpine-release is present either way
// and a step that quietly ran outside a container would pass. What separates
// them is what the fixture ADDED — the worker has an sshd installed and the
// stock image does not.
//
// "Did it see the right tree" is the one that silently fails. Docker answers a
// bind mount it cannot resolve by creating an empty directory, so a step that
// mounted a path the worker's daemon does not have succeeds and produces
// nothing. Reading an uploaded file back makes that loud.
func TestLinuxRootWorkerRunsAContainerizedStep(t *testing.T) {
	t.Parallel()

	worker := startLinuxWorkerWithDocker(t, true)

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner, err := NewRunner(shell.RunnerSpec{
		Cwd:       cwd,
		Worker:    worker.url,
		WorkerTag: "linux",
		Image:     containerizedStepImage,
		Fetch:     []string{"out"},
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	// cat FAILS on an unresolved bind mount, so a wrong mount path fails the
	// step here rather than producing a report to inspect.
	err = runner.Run(context.Background(),
		`cat data/seed.txt > out/report.txt; `+
			`if [ -e /usr/sbin/sshd ]; then echo host; else echo container; fi >> out/report.txt; `+
			`echo "$STEPS_WORKER" >> out/report.txt`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Read back on THIS machine, which is the last link: the outputs were
	// written inside a container, on a worker, and fetched home.
	report := mustRead(t, filepath.Join(cwd, "out", "report.txt"))

	const want = "seed\ncontainer\nlinux\n"
	if report != want {
		t.Errorf("report = %q, want %q", report, want)
	}
}

// TestLinuxRootWorkerPullsAPlacedImage pins who fetches the image a placed
// containerized step runs in.
//
// Nothing on this machine can. Config.Images() deliberately omits a placed
// step's image and says why: the worker does not exist yet when the pipeline
// is asked, so pulling here would download it to a machine that will never run
// it. The image therefore has to arrive on the WORKER's daemon, and the only
// moment that can happen is when the container is created.
//
// `docker run` did it implicitly and nobody had to think about it. Creating a
// container over the engine API does not — it answers a missing image with a
// 404 — so a step on a freshly acquired machine would die on an image the old
// implementation would have fetched, after the machine was launched and billed
// and the tree pushed.
//
// The worker's daemon is deliberately NOT seeded here, which is the whole
// test: it starts empty, and the step only runs if something pulled.
func TestLinuxRootWorkerPullsAPlacedImage(t *testing.T) {
	t.Parallel()

	worker := startLinuxWorkerWithDocker(t, false)

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner, err := NewRunner(shell.RunnerSpec{
		Cwd:       cwd,
		Worker:    worker.url,
		WorkerTag: "linux",
		Image:     containerizedStepImage,
		Fetch:     []string{"out"},
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	err = runner.Run(context.Background(), `cat data/seed.txt > out/report.txt`)
	if err != nil {
		t.Fatalf("Run: %v; the worker's daemon started empty, so the image was never pulled", err)
	}

	if report := mustRead(t, filepath.Join(cwd, "out", "report.txt")); report != "seed\n" {
		t.Errorf("report = %q, want the step to have run in the pulled image", report)
	}
}
