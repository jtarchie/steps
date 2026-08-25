package venue

// The ssh: venue against a real SSH server, running in this process.
//
// Every one of these pushes a binary over sftp for real, execs it through a
// shell for real, and speaks the protocol over a real SSH channel. The binary
// pushed is this test binary, which answers to _shim (see TestMain) — the
// os/exec helper-process pattern, so nothing about the transport is stubbed.

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
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
