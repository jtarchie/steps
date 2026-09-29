package venue

// The ssh: venue against a real SSH server, running in this process.
//
// Every one of these pushes a binary over sftp for real, execs it through a
// shell for real, and speaks the protocol over a real SSH channel. The binary
// pushed is this test binary, which answers to _shim (see TestMain) — the
// os/exec helper-process pattern, so nothing about the transport is stubbed.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/shim"
)

func sshSpec(t *testing.T, server *testSSHD, cwd string, outputs ...string) shell.RunnerSpec {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	return shell.RunnerSpec{
		Cwd: cwd,
		// binary= names what to push. Under `go test` this process IS the test
		// binary, which is exactly what a worker needs to run.
		Worker:    server.URL + "&binary=" + self,
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

	runner, err := NewRunner(sshSpec(t, server, cwd, "out"))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	err = runner.Run(context.Background(), `cat data/seed.txt > out/report.txt; echo "$STEPS_WORKER" >> out/report.txt`)
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
}

// TestSSHWorkerPushesTheBinaryOnceAndReusesIt pins the cache. A ~50MB upload
// per step would make the feature unusable on anything but a LAN, and the
// content-keyed path is what stops it.
func TestSSHWorkerPushesTheBinaryOnceAndReusesIt(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)

	for range 2 {
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
	}

	if pushed := uploadsUnder(t, server.Root); pushed != 1 {
		t.Errorf("%d binaries on the worker, want exactly 1 — the second session did not reuse the first push", pushed)
	}

	if server.Execs.Load() < 2 {
		t.Errorf("Execs = %d, want at least one per session", server.Execs.Load())
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

	// The binary stays — it is the cache. Nothing else should.
	sessions := filepath.Join(server.Root, "steps-shim")

	entries, err := os.ReadDir(sessions)
	if err != nil {
		t.Fatalf("reading %q: %v", sessions, err)
	}

	for _, entry := range entries {
		work := filepath.Join(sessions, entry.Name(), "work")

		_, err = os.Stat(work)
		if err == nil {
			t.Errorf("a session work directory outlived the step: %s", work)
		}
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

// TestSSHWorkerReportsAPushedBinaryThatCannotRun covers the failure an
// architecture mismatch produces. The worker's shell writes to the channel's
// stderr, which carries no protocol bytes precisely so this message survives
// to be reported.
func TestSSHWorkerReportsAPushedBinaryThatCannotRun(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)

	// A "binary" that is not one, standing in for one built for another
	// architecture: the far end refuses to exec it either way.
	bogus := filepath.Join(t.TempDir(), "steps")

	err := os.WriteFile(bogus, []byte("not a binary\n"), 0o700) //nolint:gosec // the point is a file the worker will try to exec
	if err != nil {
		t.Fatalf("writing a bogus binary: %v", err)
	}

	spec := sshSpec(t, server, t.TempDir())
	spec.Worker = server.URL + "&binary=" + bogus

	runner, err := NewRunner(spec)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	err = runner.Run(context.Background(), "true")
	if err == nil {
		t.Fatal("a worker running a binary that cannot exec reported success")
	}

	if shell.IsExitError(err) {
		t.Error("a shim that never started classified as the command's own exit")
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

// TestSSHWorkerRunsANamedShimWithoutPushing is ?shim= on ssh://: a shim
// already on the worker, so nothing is transferred — not even an sftp
// session opened to decide whether to.
func TestSSHWorkerRunsANamedShimWithoutPushing(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	runner, err := NewRunner(shell.RunnerSpec{
		Cwd:       t.TempDir(),
		Worker:    server.URL + "&shim=" + url.QueryEscape(self),
		WorkerTag: "baked",
	})
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

	if pushed := uploadsUnder(t, server.Root); pushed != 0 {
		t.Errorf("%d binaries pushed, want none — ?shim= names one already there", pushed)
	}

	if opened := server.Subsystems.Load(); opened != 0 {
		t.Errorf("%d sftp sessions, want none", opened)
	}
}

// TestSSHWorkerBlamesAMissingShimOnTheMapping pins the hint: a ?shim= that
// does not run is the path the operator wrote, not a binary to rebuild.
func TestSSHWorkerBlamesAMissingShimOnTheMapping(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)

	runner, err := NewRunner(shell.RunnerSpec{
		Cwd:       t.TempDir(),
		Worker:    server.URL + "&shim=/nonexistent",
		WorkerTag: "baked",
		NoRedial:  true,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	err = runner.Run(context.Background(), "true")
	if !errors.Is(err, errShimDidNotStart) {
		t.Fatalf("Run = %v, want errShimDidNotStart", err)
	}

	if !strings.Contains(err.Error(), "?shim=/nonexistent") || strings.Contains(err.Error(), "?binary=") {
		t.Errorf("Run = %v, want the ?shim= path blamed and no ?binary= advice", err)
	}
}

// TestSSHWorkerPushesTheEmbeddedShim is the pick crossing into the push: the
// worker reports this machine's platform, a shim is embedded for it, and
// that — not this process — is what travels, once, with the push said once.
// Not parallel: it installs embedded shims.
func TestSSHWorkerPushesTheEmbeddedShim(t *testing.T) {
	server := newTestSSHD(t)

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	binary, err := os.ReadFile(self) //nolint:gosec // the test binary, standing in for a shim built for this platform
	if err != nil {
		t.Fatalf("reading the test binary: %v", err)
	}

	name := shimName(runtime.GOOS, runtime.GOARCH)
	withShims(t, map[string][]byte{name: binary})

	var notes bytes.Buffer

	runOnce(t, server.URL, &notes)
	runOnce(t, server.URL, &notes)

	if pushed := uploadsUnder(t, server.Root); pushed != 1 {
		t.Errorf("%d binaries on the worker, want exactly 1", pushed)
	}

	info, err := os.Stat(filepath.Join(server.Root, "steps-shim", shim.BuildOfBytes(binary), "steps")) //nolint:gosec // a path under this test's own worker root
	if err != nil || info.Size() != int64(len(binary)) {
		t.Errorf("pushed shim = %v, %v, want the embedded bytes filed under their own hash", info, err)
	}

	if said := strings.Count(notes.String(), "pushing shim "+name); said != 1 {
		t.Errorf("notes = %q, want the embedded shim's push said exactly once", notes.String())
	}
}

// TestSSHWorkerRefusesAPlantedShim pins the planting defence: a same-size
// file at the content-keyed path, under a directory anyone could write, is
// refused by name and never executed.
func TestSSHWorkerRefusesAPlantedShim(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	info, err := os.Stat(self)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	build, err := shim.SelfBuild()
	if err != nil {
		t.Fatalf("SelfBuild: %v", err)
	}

	dir := filepath.Join(server.Root, "steps-shim", build)
	mustMkdir(t, dir)

	err = os.Chmod(dir, 0o777) //nolint:gosec // the point: a directory another login could have written
	if err != nil {
		t.Fatalf("chmod: %v", err)
	}

	marker := filepath.Join(t.TempDir(), "ran")
	planted := "#!/bin/sh\ntouch " + marker + "\n"
	planted += strings.Repeat("#", int(info.Size())-len(planted))

	err = os.WriteFile(filepath.Join(dir, "steps"), []byte(planted), 0o700) //nolint:gosec // an executable, as a planted shim would be
	if err != nil {
		t.Fatalf("planting: %v", err)
	}

	runner, err := NewRunner(shell.RunnerSpec{Cwd: t.TempDir(), Worker: server.URL, WorkerTag: "gpu", NoRedial: true})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	err = runner.Run(context.Background(), "true")
	if !errors.Is(err, errShimNotPrivate) || !strings.Contains(err.Error(), dir) {
		t.Errorf("Run = %v, want the planted path refused by name", err)
	}

	_, err = os.Stat(marker)
	if err == nil {
		t.Error("the planted shim ran")
	}
}

// runOnce runs one trivial step on a fresh session to worker, with the
// run's notes printed to notes.
func runOnce(t *testing.T, worker string, notes io.Writer) {
	t.Helper()

	ctx := events.WithOutput(context.Background(), events.Output{Stdout: notes})

	runner, err := NewRunner(shell.RunnerSpec{Cwd: t.TempDir(), Worker: worker, WorkerTag: "gpu"})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	err = runner.Run(ctx, "true")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	err = runner.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}
