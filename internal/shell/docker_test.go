package shell

// A step's container, asked of a real daemon.
//
// This file used to build argument vectors and assert on their contents,
// because that was all a test could reach without a daemon: the container
// implementation spawned `docker`, so the argv was the only artefact. It no
// longer builds one. What each of those tests was really asking — did the
// mount reach the container, did the limit take effect, did a nonzero exit
// stay data — is asked of the daemon now, and the answers mean the same thing
// they always did rather than one indirection short of it.
//
// The flag-block assertions that survive as argv tests live in
// dockerrun_test.go, which covers the one foreground `docker run` left.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestNewContainerNameIsUnique guards the reason names are random rather than
// derived from a step's name: two concurrent runs of the same step must never
// contend for one container.
func TestNewContainerNameIsUnique(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}

	for range 100 {
		name, err := NewContainerName()
		if err != nil {
			t.Fatalf("NewContainerName: %v", err)
		}

		if !strings.HasPrefix(name, "steps-") {
			t.Errorf("name = %q, want a steps- prefix", name)
		}

		if seen[name] {
			t.Fatalf("NewContainerName returned %q twice", name)
		}

		seen[name] = true
	}
}

// TestKeepaliveIsBounded pins that an abandoned container stops on its own.
//
// A steps process killed outright never runs Close, and before the sweep
// existed the keepalive expiring was the only thing that ever reclaimed the
// container. An endless loop would hold memory until someone noticed; a bound
// far above any plausible step duration cannot cut a real one short.
func TestKeepaliveIsBounded(t *testing.T) {
	t.Parallel()

	command := keepAliveCommand()

	seconds, ok := strings.CutPrefix(command, "sleep ")
	if !ok {
		t.Fatalf("keepAliveCommand() = %q, want a bounded sleep", command)
	}

	parsed, err := strconv.Atoi(seconds)
	if err != nil {
		t.Fatalf("keepAliveCommand() = %q, want a number of seconds: %v", command, err)
	}

	if time.Duration(parsed)*time.Second != dockerSessionLifetime {
		t.Errorf("keepalive sleeps %ds, want %s", parsed, dockerSessionLifetime)
	}
}

// TestResolveMountPathRejectsColonInPath guards against docker's
// `host:container` volume spec silently misparsing a host path that itself
// contains a ':' (a valid POSIX path character). It must fail loudly at
// construction rather than produce a mount nobody asked for.
func TestResolveMountPathRejectsColonInPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cwd := filepath.Join(dir, "weird:name")

	err := os.Mkdir(cwd, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	_, err = ResolveMountPath(cwd)
	if err == nil {
		t.Error("expected an error for a working directory containing ':'")
	}
}

// TestNewRunnerRejectsColonInPath confirms the colon rejection surfaces
// through NewRunner (where it's actually triggered in production), not just
// the internal ResolveMountPath helper.
func TestNewRunnerRejectsColonInPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cwd := filepath.Join(dir, "weird:name")

	err := os.Mkdir(cwd, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	_, err = NewRunner(RunnerSpec{Image: "alpine", Cwd: cwd})
	if err == nil {
		t.Error("expected an error for a working directory containing ':'")
	}
}

func TestNewRunner(t *testing.T) {
	t.Parallel()

	hostRunner, err := NewRunner(RunnerSpec{Cwd: "somedir"})
	if err != nil {
		t.Fatalf("NewRunner(\"\", ...): %v", err)
	}

	if _, ok := hostRunner.(HostRunner); !ok {
		t.Error("NewRunner(\"\", ...) should return a HostRunner")
	}

	dockerRunnerIface, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner(\"alpine\", ...): %v", err)
	}

	runner, ok := dockerRunnerIface.(DockerRunner)
	if !ok {
		t.Fatal("NewRunner(\"alpine\", ...) should return a DockerRunner")
	}

	if runner.Image != "alpine" {
		t.Errorf("Image = %q, want alpine", runner.Image)
	}
}

// TestNewRunnerResolvesCwdOnce guards the whole point of construction-time
// resolution: NewRunner's returned DockerRunner carries an already-resolved
// cwd, so Run/RunCapture/RunCaptureFull never need to re-resolve it.
func TestNewRunnerResolvesCwdOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	runnerIface, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: dir})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	runner, ok := runnerIface.(DockerRunner)
	if !ok {
		t.Fatal("expected a DockerRunner")
	}

	want, err := ResolveMountPath(dir)
	if err != nil {
		t.Fatal(err)
	}

	if runner.session.resolvedCwd != want {
		t.Errorf("resolvedCwd = %q, want %q", runner.session.resolvedCwd, want)
	}
}

// ourContainerCount reports how many containers this process currently owns,
// by the labels every one of them carries.
func ourContainerCount(t *testing.T) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	//nolint:gosec // fixed argv but for this process's own pid
	out, err := exec.CommandContext(ctx, "docker", "ps", "--all", "--quiet",
		"--filter", "label="+dockerOwnerLabel+"=steps",
		"--filter", "label="+dockerPIDLabel+"="+strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Fatalf("listing this process's containers: %v", err)
	}

	return len(strings.Fields(string(out)))
}

// TestNewRunnerDoesNotStartAContainer pins the laziness: constructing a runner
// must cost nothing. A step whose command is skipped, or which fails before
// running anything, should never have paid for a container.
func TestNewRunnerDoesNotStartAContainer(t *testing.T) {
	requireDocker(t)

	before := ourContainerCount(t)

	runner, err := NewRunner(RunnerSpec{Image: testImage, Cwd: mountableTempDir(t)})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	if after := ourContainerCount(t); after != before {
		t.Errorf("container count went %d -> %d; NewRunner must not start one until the first command", before, after)
	}
}

// TestDockerRunnerCaptureFull pins the shape every non-interactive caller
// depends on: both streams kept apart, and a nonzero exit reported as data
// rather than as a Go error.
func TestDockerRunnerCaptureFull(t *testing.T) {
	requireDocker(t)

	runner := newTestRunner(t, RunnerSpec{Image: testImage, Cwd: mountableTempDir(t)})

	stdout, stderr, exitCode, err := runner.RunCaptureFull(context.Background(),
		"printf 'out text'; printf 'err text' >&2; exit 3")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	if exitCode != 3 {
		t.Errorf("exitCode = %d, want 3 (a normal nonzero exit must be data, not an error)", exitCode)
	}

	if stdout != "out text" {
		t.Errorf("stdout = %q, want %q", stdout, "out text")
	}

	if stderr != "err text" {
		t.Errorf("stderr = %q, want %q", stderr, "err text")
	}
}

// newTestRunner builds a runner and closes it when the test ends.
func newTestRunner(t *testing.T, spec RunnerSpec) Runner {
	t.Helper()

	runner, err := NewRunner(spec)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { CloseRunner(runner, "test") })

	return runner
}

// TestDockerRunnerReusesOneContainer is the point of the whole session: two
// commands from one runner land in the SAME container, so state a model
// established in one run_shell call is still there for the next.
func TestDockerRunnerReusesOneContainer(t *testing.T) {
	requireDocker(t)

	before := ourContainerCount(t)
	runner := newTestRunner(t, RunnerSpec{Image: testImage, Cwd: mountableTempDir(t)})

	for _, command := range []string{"echo one > /tmp/marker", "cat /tmp/marker"} {
		_, _, code, err := runner.RunCaptureFull(context.Background(), command)
		if err != nil || code != 0 {
			t.Fatalf("%q: exit %d, err %v", command, code, err)
		}
	}

	if after := ourContainerCount(t); after != before+1 {
		t.Errorf("container count went %d -> %d; two commands must share one container", before, after)
	}
}

// TestDockerRunnerWithLabelSharesOneContainer pins that a labelled copy is the
// same step's container and not a second one — WithLabel changes how output is
// printed, nothing else.
func TestDockerRunnerWithLabelSharesOneContainer(t *testing.T) {
	requireDocker(t)

	before := ourContainerCount(t)
	runner := newTestRunner(t, RunnerSpec{Image: testImage, Cwd: mountableTempDir(t)})

	_, _, _, err := runner.RunCaptureFull(context.Background(), "true")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	_, _, _, err = runner.WithLabel("labelled").RunCaptureFull(context.Background(), "true")
	if err != nil {
		t.Fatalf("RunCaptureFull through a labelled runner: %v", err)
	}

	if after := ourContainerCount(t); after != before+1 {
		t.Errorf("container count went %d -> %d; a labelled runner must reuse the same container", before, after)
	}
}

// TestDockerRunnerCloseWithoutCommandsRemovesNothing pins that closing a
// runner that never ran anything is safe — there was no container to remove.
func TestDockerRunnerCloseWithoutCommandsRemovesNothing(t *testing.T) {
	requireDocker(t)

	runner, err := NewRunner(RunnerSpec{Image: testImage, Cwd: mountableTempDir(t)})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	err = runner.Close()
	if err != nil {
		t.Errorf("Close: %v, want closing an unused runner to be a no-op", err)
	}
}

// TestDockerRunnerCloseIsIdempotent pins that a second Close is harmless, so
// a deferred one beside an explicit one cannot report a failure.
func TestDockerRunnerCloseIsIdempotent(t *testing.T) {
	requireDocker(t)

	runner, err := NewRunner(RunnerSpec{Image: testImage, Cwd: mountableTempDir(t)})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	_, _, _, err = runner.RunCaptureFull(context.Background(), "true")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	for attempt := range 2 {
		err = runner.Close()
		if err != nil {
			t.Errorf("Close %d: %v", attempt, err)
		}
	}
}

// TestDockerRunnerCaptureFullPreservesAdversarialOutput pins that a command's
// bytes come back as it wrote them.
//
// This used to guard the fake docker script's own quoting, which no longer
// exists — but the question outlived it: the daemon frames both streams over
// one connection, and a reader that mishandled the framing would mangle
// exactly this kind of content.
func TestDockerRunnerCaptureFullPreservesAdversarialOutput(t *testing.T) {
	requireDocker(t)

	const adversarial = `it's a "test" with \backslashes\ and 'quotes'`

	runner := newTestRunner(t, RunnerSpec{Image: testImage, Cwd: mountableTempDir(t)})

	stdout, _, _, err := runner.RunCaptureFull(context.Background(), "cat /tmp/adversarial 2>/dev/null || printf '%s' "+singleQuote(adversarial))
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	if stdout != adversarial {
		t.Errorf("stdout = %q, want %q", stdout, adversarial)
	}
}

// singleQuote wraps a value so a POSIX shell reproduces it verbatim.
func singleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func TestDockerRunnerRunErrorsOnNonzeroExit(t *testing.T) {
	requireDocker(t)

	runner := newTestRunner(t, RunnerSpec{Image: testImage, Cwd: mountableTempDir(t)})

	err := runner.Run(context.Background(), "false")
	if err == nil {
		t.Fatal("expected an error for a nonzero exit from Run (unlike RunCaptureFull)")
	}

	// The step said no, which is not the same as the machinery breaking.
	if !IsExitError(err) {
		t.Errorf("IsExitError(%v) = false, want a command's own nonzero exit to classify as a failure", err)
	}
}

func TestDockerRunnerRunCaptureReturnsStdout(t *testing.T) {
	requireDocker(t)

	runner := newTestRunner(t, RunnerSpec{Image: testImage, Cwd: mountableTempDir(t)})

	out, err := runner.RunCapture(context.Background(), "printf 'captured output'")
	if err != nil {
		t.Fatalf("RunCapture: %v", err)
	}

	if string(out) != "captured output" {
		t.Errorf("out = %q, want %q", out, "captured output")
	}
}

// TestDockerRunnerRunCaptureLogsStderrOnFailure guards against a regression
// where RunCapture streamed stderr live but never captured it for the debug
// log, unlike HostRunner.RunCapture — which exists so a failing check/out
// command's output is available for debugging rather than discarded.
func TestDockerRunnerRunCaptureLogsStderrOnFailure(t *testing.T) {
	requireDocker(t)

	runner := newTestRunner(t, RunnerSpec{Image: testImage, Cwd: mountableTempDir(t)})

	var logBuf bytes.Buffer

	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	defer slog.SetDefault(prevLogger)

	_, err := runner.RunCapture(context.Background(), "printf 'boom from stderr' >&2; false")
	if err == nil {
		t.Fatal("expected an error for a nonzero exit")
	}

	if !strings.Contains(logBuf.String(), "boom from stderr") {
		t.Errorf("debug log = %q, want it to contain the captured stderr", logBuf.String())
	}
}

// entrypointImage builds an image whose ENTRYPOINT swallows any command given
// to it, which is the shape that makes a session container die at birth.
func entrypointImage(t *testing.T) string {
	t.Helper()

	const name = "steps-test-entrypoint:latest"

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "build", "-q", "-t", name, "-")
	cmd.Stdin = strings.NewReader("FROM " + testImage + "\nENTRYPOINT [\"/bin/echo\"]\n")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("cannot build the entrypoint fixture image: %v\n%s", err, out)
	}

	return name
}

// TestDockerRunnerDetectsAContainerThatDiedAtBirth is the case starting a
// container cannot report: it succeeds for one that stopped a millisecond
// later, which is the NORMAL outcome for an image with an ENTRYPOINT, since
// the keepalive becomes arguments to that entrypoint instead of replacing it.
// Trusting that success left every later exec saying the container did not
// exist, which names neither the image nor the reason.
func TestDockerRunnerDetectsAContainerThatDiedAtBirth(t *testing.T) {
	requireDocker(t)

	image := entrypointImage(t)
	runner := newTestRunner(t, RunnerSpec{Image: image, Cwd: mountableTempDir(t)})

	_, _, _, err := runner.RunCaptureFull(t.Context(), "anything")
	if err == nil {
		t.Fatal("expected an error when the container did not stay up")
	}

	for _, want := range []string{image, "exited immediately"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}

	// A container that never accepted a command is INFRASTRUCTURE, not the
	// step saying no. Classifying it as a failure would route it to
	// on_failure and let a guard read an unusable image as "the guard said
	// false", silently skipping the work it gates.
	if IsExitError(err) {
		t.Errorf("IsExitError(%v) = true, want a container that never ran anything to stay an infrastructure error", err)
	}
}

// TestRunOnAContainerThatDiedAtBirthIsNotAnExitError is the same
// classification asked through the OTHER door.
//
// RunCaptureFull reports a container that never started as a Go error, and the
// test above pins that. Run and RunCapture take a different path — they turn a
// nonzero exit into an error too, so both outcomes arrive as one — and that is
// the path a guard uses. Misclassified there, an image that cannot run at all
// reads as "the guard said false", and the work it gates is skipped with
// nothing red anywhere.
func TestRunOnAContainerThatDiedAtBirthIsNotAnExitError(t *testing.T) {
	requireDocker(t)

	runner := newTestRunner(t, RunnerSpec{Image: entrypointImage(t), Cwd: mountableTempDir(t)})

	runErr := runner.Run(t.Context(), "true")
	if runErr == nil {
		t.Fatal("Run succeeded against a container that never stayed up")
	}

	if IsExitError(runErr) {
		t.Errorf("IsExitError(%v) = true; a guard would read an unusable image as the guard answering no", runErr)
	}

	_, captureErr := runner.RunCapture(t.Context(), "true")
	if captureErr == nil {
		t.Fatal("RunCapture succeeded against a container that never stayed up")
	}

	if IsExitError(captureErr) {
		t.Errorf("IsExitError(%v) = true through RunCapture", captureErr)
	}
}

// TestDockerRunnerRemovesAContainerThatDiedAtBirth pins that diagnosing the
// corpse does not mean keeping it: the container is not self-removing
// precisely so the postmortem survives long enough to read, which makes
// taking it away afterwards our job.
func TestDockerRunnerRemovesAContainerThatDiedAtBirth(t *testing.T) {
	requireDocker(t)

	before := ourContainerCount(t)
	runner := newTestRunner(t, RunnerSpec{Image: entrypointImage(t), Cwd: mountableTempDir(t)})

	_, _, _, _ = runner.RunCaptureFull(t.Context(), "anything")

	if after := ourContainerCount(t); after != before {
		t.Errorf("container count went %d -> %d; the dead container was left behind", before, after)
	}
}

// TestDockerRunnerRejectsCommandsAfterClose guards the aliasing hazard Close
// creates: it clears the container id, so without an explicit closed flag the
// next command would exec against an empty one and report a malformed request
// as an ordinary exit code.
func TestDockerRunnerRejectsCommandsAfterClose(t *testing.T) {
	requireDocker(t)

	runner, err := NewRunner(RunnerSpec{Image: testImage, Cwd: mountableTempDir(t)})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	_, _, _, err = runner.RunCaptureFull(t.Context(), "true")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	err = runner.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, _, _, err = runner.RunCaptureFull(t.Context(), "echo after close")
	if !errors.Is(err, errSessionClosed) {
		t.Errorf("err = %v, want errSessionClosed", err)
	}
}

// TestDockerRunnerEmptyCwdMountsNothing covers the one caller that has no
// working directory — a resource check: — which must run in the image's own
// workdir rather than mounting something arbitrary.
func TestDockerRunnerEmptyCwdMountsNothing(t *testing.T) {
	requireDocker(t)

	runner := newTestRunner(t, RunnerSpec{Image: testImage})

	stdout, _, code, err := runner.RunCaptureFull(context.Background(), "pwd")
	if err != nil || code != 0 {
		t.Fatalf("RunCaptureFull: exit %d, err %v", code, err)
	}

	if strings.TrimSpace(stdout) != "/" {
		t.Errorf("pwd = %q, want the image's own working directory", strings.TrimSpace(stdout))
	}
}

// TestValidateDockerNeedsOnlyADaemon pins what preflight now asks, and — more
// to the point — what it no longer asks.
//
// There used to be two halves: the docker BINARY on PATH, and a reachable
// daemon. A PLACED containerized step needed only the first, since its
// container ran on the worker's daemon and was started by this machine's
// docker CLI through the forwarded socket. Nothing spawns that CLI any more,
// so a machine with a reachable daemon and no docker installed runs a
// containerized pipeline perfectly well, and an orchestrator with neither
// still runs placed ones.
func TestValidateDockerNeedsOnlyADaemon(t *testing.T) {
	requireDocker(t)

	t.Setenv("PATH", t.TempDir()) // no docker binary anywhere

	err := ValidateDocker(context.Background())
	if err != nil {
		t.Errorf("ValidateDocker: %v; a reachable daemon is the whole requirement", err)
	}

	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")

	err = ValidateDocker(context.Background())
	if err == nil {
		t.Error("ValidateDocker accepted a daemon that cannot be reached")
	}
}

// TestContainerUserPrefersTheConfiguredValue pins that user: always wins over
// the platform default — it is the documented escape hatch for an image that
// needs root, so a platform default silently overriding it would make the
// knob useless exactly where it matters.
func TestContainerUserPrefersTheConfiguredValue(t *testing.T) {
	t.Parallel()

	if got := containerUser("root", true); got != "root" {
		t.Errorf("containerUser(\"root\") = %q, want root", got)
	}

	if got := containerUser("1234:5678", true); got != "1234:5678" {
		t.Errorf("containerUser(\"1234:5678\") = %q, want it passed through verbatim", got)
	}
}

// TestDefaultContainerUserIsPlatformSpecific pins the split that the whole fix
// rests on: Linux bind mounts carry host uids, so a default is required there;
// Docker Desktop maps ownership already, so forcing a uid would only break
// images whose own files belong to the user they expect.
func TestDefaultContainerUserIsPlatformSpecific(t *testing.T) {
	t.Parallel()

	got := defaultContainerUser()

	if runtime.GOOS == "linux" {
		want := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
		if got != want {
			t.Errorf("defaultContainerUser() = %q, want %q on linux", got, want)
		}

		return
	}

	if got != "" {
		t.Errorf("defaultContainerUser() = %q, want empty off linux (the image's own user)", got)
	}
}
