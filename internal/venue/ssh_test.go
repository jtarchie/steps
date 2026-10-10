package venue

// The ssh: venue against a real SSH server, running in this process.
//
// Every one of these sends a tree in as a tar for real, execs the command
// through a shell for real, and brings the outputs back over a real SSH
// channel, so nothing about the transport is stubbed.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/jtarchie/steps/internal/shell"
)

func sshSpec(t *testing.T, server *testSSHD, cwd string, outputs ...string) shell.RunnerSpec {
	t.Helper()

	return shell.RunnerSpec{
		Cwd:       cwd,
		Worker:    server.URL,
		WorkerTag: "gpu",
		Fetch:     outputs,
	}
}

// TestSSHWorkerRoundTripsAStep is the feature over its real transport: the
// tree goes out, the command runs on the far side of an SSH channel, and the
// declared outputs come back.
func TestSSHWorkerRoundTripsAStep(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")
	mustMkdir(t, filepath.Join(cwd, "out"))
	mustMkdir(t, filepath.Join(cwd, "logs"))

	runner, err := NewRunner(sshSpec(t, server, cwd, "out", "logs"))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	err = runner.Run(context.Background(), `cat data/seed.txt > out/report.txt; echo "$STEPS_WORKER" >> out/report.txt; echo done > logs/run.log`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := mustRead(t, filepath.Join(cwd, "out", "report.txt"))
	if !strings.Contains(got, "seed") {
		t.Errorf("out/report.txt = %q, want the input the worker consumed", got)
	}

	if !strings.Contains(got, "gpu") {
		t.Errorf("out/report.txt = %q, want STEPS_WORKER to have reached the command", got)
	}

	if got := mustRead(t, filepath.Join(cwd, "logs", "run.log")); got != "done\n" {
		t.Errorf("logs/run.log = %q, want every declared output brought back, not only the first", got)
	}
}

// TestSSHWorkerSendsNoEnvRequests is a production-only failure this can
// otherwise not catch. OpenSSH's sshd ignores SSH env requests unless
// AcceptEnv names the variable (default: LANG and LC_*), so a venue that
// shipped a step's environment that way would pass every test here and
// silently drop the values against a real worker.
func TestSSHWorkerSendsNoEnvRequests(t *testing.T) {
	// Not parallel: it sets a variable in this process's environment, which is
	// where the venue resolves an opted-in name from.
	server := newTestSSHD(t)

	spec := sshSpec(t, server, t.TempDir())
	spec.Env = []string{"STEPS_TEST_SSH_VALUE"}

	t.Setenv("STEPS_TEST_SSH_VALUE", "carried-in-a-frame")

	runner, err := NewRunner(spec)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	stdout, _, err := runner.RunStreamedCapture(context.Background(), `echo "$STEPS_TEST_SSH_VALUE"`, 0)
	if err != nil {
		t.Fatalf("RunStreamedCapture: %v", err)
	}

	if !strings.Contains(stdout, "carried-in-a-frame") {
		t.Errorf("stdout = %q, want the opted-in value to have reached the command", stdout)
	}

	if server.EnvRequests.Load() != 0 {
		t.Errorf("the venue sent %d SSH env requests; values must travel in protocol frames, which a real sshd would have dropped",
			server.EnvRequests.Load())
	}
}

// Build metadata rides each command over ssh:// too, and the step's env stays off the worker's command line.
func TestSSHWorkerSendsBuildMetadataOffTheCommandLine(t *testing.T) {
	server := newTestSSHD(t)

	spec := sshSpec(t, server, t.TempDir())
	spec.Env = []string{"STEPS_TEST_SSH_SECRET"}

	t.Setenv("STEPS_TEST_SSH_SECRET", "hunter2")

	runner := newLocalRunner(t, spec)
	ctx := shell.WithBuildMetadata(t.Context(), shell.BuildMetadata{RunID: "run-42"})

	stdout, _, err := runner.RunStreamedCapture(ctx, `echo "$STEPS_RUN_ID $STEPS_TEST_SSH_SECRET"; ps -o args= -p $$`, 0)
	if err != nil {
		t.Fatalf("RunStreamedCapture: %v", err)
	}

	if !strings.HasPrefix(stdout, "run-42 hunter2\n") {
		t.Errorf("stdout = %q, want the run id and the env value", stdout)
	}

	if strings.Count(stdout, "hunter2") != 1 {
		t.Errorf("the env value is in the command's argv: %q", stdout)
	}
}

// TestSSHWorkerNonzeroExitIsAStepFailure pins the classification across the
// real transport, not just the local one.
func TestSSHWorkerNonzeroExitIsAStepFailure(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)

	runner, err := NewRunner(sshSpec(t, server, t.TempDir()))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	err = runner.Run(context.Background(), "exit 3")
	if err == nil {
		t.Fatal("Run succeeded on a command that exited 3")
	}

	if !shell.IsExitError(err) {
		t.Errorf("IsExitError = false over ssh: %v", err)
	}

	// A guard reads the status itself, so it must be the command's own and not a stand-in.
	_, _, code, err := runner.RunCaptureFull(context.Background(), "exit 3")
	if err != nil || code != 3 {
		t.Errorf("RunCaptureFull = %d, %v; want exit 3 as data", code, err)
	}
}

// TestSSHWorkerCleansUpItsScratch pins the promise that makes this safe to
// point at a machine that is not yours.
func TestSSHWorkerCleansUpItsScratch(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)

	runner, err := NewRunner(sshSpec(t, server, t.TempDir()))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	err = runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	err = runner.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A bare worker keeps nothing: no build directory, no owner or pid file beside it.
	entries, err := os.ReadDir(server.Root)
	if err != nil {
		t.Fatalf("reading %q: %v", server.Root, err)
	}

	for _, entry := range entries {
		t.Errorf("left on the worker after close: %s", entry.Name())
	}
}

// A scratch directory close could not remove is reported, or it sits on somebody's machine unnoticed.
func TestSSHWorkerReportsScratchItCouldNotRemove(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root removes a file under a read-only directory")
	}

	runner, err := NewRunner(sshSpec(t, newTestSSHD(t), t.TempDir()))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	err = runner.Run(t.Context(), "mkdir -p locked && touch locked/file && chmod 555 locked")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	placement, _ := runner.(bareRunner).placement()
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(placement.Workdir, "locked"), 0o755) }) //nolint:gosec // a directory the test must be able to remove

	err = runner.Close()
	if !errors.Is(err, errBareTransfer) {
		t.Errorf("Close = %v, want the failed removal reported", err)
	}
}

// TestSSHWorkerRefusesAnUnknownHostKey is the one security property this
// feature cannot be allowed to skip: a thing whose whole job is running
// commands on another machine must check which machine that is.
func TestSSHWorkerRefusesAnUnknownHostKey(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)

	// An empty known_hosts: the server's key is real, and unknown.
	empty := filepath.Join(t.TempDir(), "known_hosts")

	err := os.WriteFile(empty, nil, 0o600)
	if err != nil {
		t.Fatalf("writing an empty known_hosts: %v", err)
	}

	spec := sshSpec(t, server, t.TempDir())
	spec.Worker = replaceKnownHosts(spec.Worker, empty)

	runner, err := NewRunner(spec)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	err = runner.Run(context.Background(), "true")
	if err == nil {
		t.Fatal("the venue connected to a host whose key it had never seen")
	}

	if shell.IsExitError(err) {
		t.Error("a rejected host key classified as the command's own failure")
	}
}

// TestSSHWorkerRefusesWithoutCredentials pins that the venue says what to do
// rather than failing obscurely when there is nothing to authenticate with.
func TestSSHWorkerRefusesWithoutCredentials(t *testing.T) {
	server := newTestSSHD(t)

	// No agent, no identity: nothing to offer.
	t.Setenv("SSH_AUTH_SOCK", "")

	worker, err := ParseWorker(stripQuery(server.URL) + "?ssh_config=none")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	settings, err := connectionFor(worker)
	if err != nil {
		t.Fatalf("connectionFor: %v", err)
	}

	_, err = sshConfig(t.Context(), settings)
	if !errors.Is(err, errNoAuth) {
		t.Fatalf("error = %v, want it to name the missing credentials", err)
	}
}

// replaceKnownHosts rewrites a worker URL to check host keys against another
// file, leaving everything else about it alone.
func replaceKnownHosts(worker, path string) string {
	parsed, err := url.Parse(worker)
	if err != nil {
		return worker
	}

	query := parsed.Query()
	query.Set("known_hosts", path)
	parsed.RawQuery = query.Encode()

	return parsed.String()
}

func stripQuery(worker string) string {
	base, _, _ := strings.Cut(worker, "?")

	return base
}

// An ssh:// worker runs steps bare; image: there is refused before anything is dialled rather than run somewhere it was not asked to.
func TestSSHWorkerRefusesAnImage(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)

	spec := sshSpec(t, server, t.TempDir())
	spec.Image = "alpine:3"

	_, err := NewRunner(spec)
	if !errors.Is(err, errImageOnSSH) {
		t.Fatalf("want errImageOnSSH, got %v", err)
	}

	if server.Execs.Load() != 0 {
		t.Fatal("the worker was reached before the refusal")
	}
}

// A command that never ends must still end the step when its deadline does, and take what it started with it.
func TestSSHWorkerCancellationEndsAWedgedCommand(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)
	runner := newLocalRunner(t, sshSpec(t, server, t.TempDir()))

	ctx, cancel := context.WithTimeout(t.Context(), shortWait)
	defer cancel()

	// The worker is this machine, so the duration carries this process's pid to tell its sleep from another test process's.
	seconds := "6" + strconv.Itoa(os.Getpid())

	started := time.Now()
	err := runner.Run(ctx, "sleep "+seconds)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want it to carry the deadline", err)
	}

	// Well under killGrace: TERM ended it, so nothing waits out the grace.
	if elapsed := time.Since(started); elapsed > killGrace {
		t.Fatalf("the cancel took %s to come back", elapsed)
	}

	out, err := runner.RunCapture(t.Context(), "pgrep -f 'slee[p] "+seconds+"' | wc -l")
	if err != nil {
		t.Fatalf("checking for the cancelled command: %v", err)
	}

	if count := strings.TrimSpace(string(out)); count != "0" {
		t.Errorf("the cancelled command is still running (%s)", count)
	}
}

// A worker whose tar fails sending the tree must fail the step, not run it on whatever arrived.
func TestSSHWorkerFailsWhenTheTreeDoesNotUnpack(t *testing.T) {
	// Not parallel: it puts a tar that refuses to extract first on PATH, which the in-process sshd's commands inherit.
	realTar, err := exec.LookPath("tar")
	if err != nil {
		t.Skipf("no tar: %v", err)
	}

	bin := t.TempDir()
	mustWrite(t, filepath.Join(bin, "tar"), "#!/bin/sh\nif [ \"$1\" = -xf ]; then cat >/dev/null; echo 'disk full' >&2; exit 2; fi\nexec "+realTar+" \"$@\"\n")

	err = os.Chmod(filepath.Join(bin, "tar"), 0o755) //nolint:gosec // an executable the test runs
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "seed.txt"), "seed\n")

	err = newLocalRunner(t, sshSpec(t, newTestSSHD(t), cwd)).Run(t.Context(), "true")
	if !errors.Is(err, errBareTransfer) || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("Run = %v, want the failed unpack reported", err)
	}
}

// A worker whose tar fails packing the outputs must fail the step, not bring home part of them.
func TestSSHWorkerFailsWhenTheOutputsDoNotPack(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file, so tar does not fail on it")
	}

	cwd := t.TempDir()
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, sshSpec(t, newTestSSHD(t), cwd, "out"))

	// Readable again afterwards, or the session's own cleanup cannot remove it.
	err := runner.Run(t.Context(), "echo kept > out/kept; echo x > out/unreadable; chmod 000 out/unreadable")
	_ = runner.Run(context.Background(), "chmod 644 out/unreadable")

	if !errors.Is(err, errBareTransfer) {
		t.Errorf("Run = %v, want the failed pack reported", err)
	}
}

// cancelOnceStarted runs command until it has touched started, then cancels it and waits for the cancel to come back.
func cancelOnceStarted(t *testing.T, runner shell.Runner, command string) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- runner.Run(ctx, command) }()

	deadline := time.Now().Add(20 * time.Second)
	for runner.Run(t.Context(), "test -f started") != nil {
		if time.Now().After(deadline) {
			t.Fatal("the command never started")
		}

		time.Sleep(50 * time.Millisecond)
	}

	cancel()

	err := <-done
	if err == nil {
		t.Fatal("the cancelled command reported success")
	}
}

// TERM comes first and gets its grace: a command cleaning up on TERM must be allowed to finish before KILL.
func TestSSHWorkerCancelGivesTERMItsGrace(t *testing.T) {
	t.Parallel()

	runner := newLocalRunner(t, sshSpec(t, newTestSSHD(t), t.TempDir()))

	cancelOnceStarted(t, runner, `trap 'sleep 0.5; touch cleaned; exit 0' TERM; touch started; while :; do sleep 1; done`)

	if runner.Run(t.Context(), "test -f cleaned") != nil {
		t.Error("the command's TERM handler never finished: KILL came before its grace ran out")
	}
}

// KILL follows the grace: a command that ignores TERM must not outlive its cancel.
func TestSSHWorkerCancelKillsACommandIgnoringTERM(t *testing.T) {
	t.Parallel()

	runner := newLocalRunner(t, sshSpec(t, newTestSSHD(t), t.TempDir()))
	// The worker is this machine; the pid tells this process's command from another test process's.
	token := "ignoresterm" + strconv.Itoa(os.Getpid())

	cancelOnceStarted(t, runner, `: `+token+`; trap '' TERM; touch started; while :; do sleep 1; done`)

	// The bracket keeps the pattern from matching the sh that runs it.
	out, err := runner.RunCapture(t.Context(), "pgrep -f 'ignoreste[r]m"+strconv.Itoa(os.Getpid())+"' | wc -l")
	if err != nil {
		t.Fatalf("checking for the cancelled command: %v", err)
	}

	if count := strings.TrimSpace(string(out)); count != "0" {
		t.Errorf("a command ignoring TERM is still running (%s) after its cancel", count)
	}
}

// A step directory a dead steps process on this machine left is removed by the next session there.
func TestSSHWorkerSweepsWhatADeadProcessLeft(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)

	dead := exec.CommandContext(t.Context(), "true")

	err := dead.Run()
	if err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(server.Root, "steps-build.stale")
	writeFile(t, stale+".owner", shell.OwnerHost()+" "+strconv.Itoa(dead.Process.Pid)+"\n")

	err = os.Mkdir(stale, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	live := filepath.Join(server.Root, "steps-build.live")
	writeFile(t, live+".owner", shell.OwnerHost()+" "+strconv.Itoa(os.Getpid())+"\n")

	runner := newLocalRunner(t, sshSpec(t, server, t.TempDir()))

	err = runner.Run(t.Context(), "true")
	if err != nil {
		t.Fatal(err)
	}

	_, err = os.Stat(stale)
	if err == nil {
		t.Error("a dead process's step directory survived the sweep")
	}

	_, err = os.Stat(live + ".owner")
	if err != nil {
		t.Error("a live process's step directory was swept")
	}
}

// --keep-workspace keeps the directory and drops its owner file: left in place, the next session from this machine would sweep it once this process exits.
func TestSSHWorkerKeepsADirectoryNothingWillSweep(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)

	spec := sshSpec(t, server, t.TempDir())
	spec.Keep = true

	runner := newLocalRunner(t, spec)

	err := runner.Run(t.Context(), "touch kept")
	if err != nil {
		t.Fatal(err)
	}

	placement, _ := PlacementOf(runner)

	err = runner.Close()
	if err != nil {
		t.Fatal(err)
	}

	_, err = os.Stat(filepath.Join(placement.Workdir, "kept"))
	if err != nil {
		t.Fatalf("the kept directory is gone: %v", err)
	}

	_, err = os.Stat(placement.Workdir + ".owner")
	if err == nil {
		t.Fatal("the kept directory still has an owner file a later sweep would act on")
	}
}

// A dropped connection costs one command, not the step: the next one dials again and finds the tree where the worker's directory kept it.
func TestSSHWorkerRedialsADroppedConnection(t *testing.T) {
	t.Parallel()

	runner, err := NewRunner(sshSpec(t, newTestSSHD(t), t.TempDir()))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	err = runner.Run(t.Context(), "echo kept > marker")
	if err != nil {
		t.Fatalf("first command: %v", err)
	}

	bare, ok := runner.(bareRunner)
	if !ok {
		t.Fatalf("runner is %T, want an ssh:// runner", runner)
	}

	_ = bare.s.conn.current().Close()

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

// Close after a drop with no command since still reaches the worker: the directory, and any command the drop interrupted, would otherwise outlive the step.
func TestSSHWorkerRemovesItsDirectoryAfterADroppedConnection(t *testing.T) {
	t.Parallel()

	runner, err := NewRunner(sshSpec(t, newTestSSHD(t), t.TempDir()))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	err = runner.Run(t.Context(), "true")
	if err != nil {
		t.Fatalf("first command: %v", err)
	}

	bare, ok := runner.(bareRunner)
	if !ok {
		t.Fatalf("runner is %T, want an ssh:// runner", runner)
	}

	dir := bare.s.dir
	dropAndWait(t, &bare.s.conn)

	err = runner.Close()
	if err != nil {
		t.Fatalf("closing after the connection dropped: %v", err)
	}

	_, err = os.Stat(dir)
	if err == nil {
		t.Errorf("%s survived its session's close", dir)
	}
}

// A connection that died under any exchange, an output fetch included, is suspect: the retry can arrive before the connection reports its end.
func TestSSHWorkerSuspectsAConnectionAnyExchangeFailedOn(t *testing.T) {
	t.Parallel()

	runner := newLocalRunner(t, sshSpec(t, newTestSSHD(t), t.TempDir()))

	err := runner.Run(t.Context(), "true")
	if err != nil {
		t.Fatalf("first command: %v", err)
	}

	bare, ok := runner.(bareRunner)
	if !ok {
		t.Fatalf("runner is %T, want an ssh:// runner", runner)
	}

	// A suspect connection costs the next command a probe, so exchanges that succeeded must not leave one.
	if bare.s.conn.suspect.Load() {
		t.Fatal("exchanges that succeeded left the connection suspected")
	}

	_ = bare.s.conn.current().Close()

	_, err = bare.s.exec(t.Context(), "true", nil, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("an exec over a closed connection succeeded")
	}

	if !bare.s.conn.suspect.Load() {
		t.Error("the failed exchange left the connection unsuspected")
	}
}

// A redial meets the tunnel that just went silent: a handshake nobody answers ends with the caller's context, not never.
func TestSSHDialGivesUpOnASilentHandshake(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}

			t.Cleanup(func() { _ = conn.Close() })
		}
	}()

	worker, err := ParseWorker("ssh://" + listener.Addr().String() + server.Root + "?" + url.Values{
		"identity":   {server.Identity},
		"hostkey":    {ssh.FingerprintSHA256(server.HostKey)},
		"ssh_config": {"none"},
	}.Encode())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	answered := make(chan error, 1)

	go func() {
		client, dialErr := sshClientFor(ctx, worker)
		if client != nil {
			_ = client.Close()
		}

		answered <- dialErr
	}()

	select {
	case err = <-answered:
		if err == nil {
			t.Error("a handshake nobody answered succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the dial outlived its context by seconds")
	}
}

// The same race on ssh://: the command the drop interrupted is killed by its pid file before the retry runs.
func TestSSHWorkerKillsTheCommandADropInterrupted(t *testing.T) {
	t.Parallel()

	runner, err := NewRunner(sshSpec(t, newTestSSHD(t), t.TempDir()))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	bare, ok := runner.(bareRunner)
	if !ok {
		t.Fatalf("runner is %T, want an ssh:// runner", runner)
	}

	assertInterruptedCommandIsKilled(t, runner, "42", func() { dropAndWait(t, &bare.s.conn) })
}
