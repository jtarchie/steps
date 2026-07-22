package shell

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestDockerRunArgsMountsCwd and its siblings below each assert one facet of
// dockerRunArgs' argv construction; split into separate top-level functions
// (rather than t.Run subtests of one function) to stay under the linter's
// per-function cyclomatic-complexity budget. dockerRunArgs takes an
// already-resolved cwd (resolution now happens once in NewRunner, not
// here — see resolveMountPath) so these tests pass a plain absolute path
// directly rather than exercising resolution.
func TestDockerRunArgsMountsCwd(t *testing.T) {
	t.Parallel()

	resolvedCwd := t.TempDir()

	args := dockerRunArgs("alpine", "echo hi", resolvedCwd, true)

	want := []string{"run", "--rm", "--init", "-i", "-v", resolvedCwd + ":" + resolvedCwd, "-w", resolvedCwd, "--", "alpine", "sh", "-c", "echo hi"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestDockerRunArgsStdinFalseOmitsDashI(t *testing.T) {
	t.Parallel()

	args := dockerRunArgs("alpine", "echo hi", t.TempDir(), false)

	if slices.Contains(args, "-i") {
		t.Errorf("args = %v, did not want -i", args)
	}
}

func TestDockerRunArgsNeverPassesDashTWithStdin(t *testing.T) {
	t.Parallel()

	args := dockerRunArgs("alpine", "echo hi", t.TempDir(), true)

	if slices.Contains(args, "-t") {
		t.Errorf("args = %v, did not want -t", args)
	}
}

func TestDockerRunArgsNeverPassesDashTWithoutStdin(t *testing.T) {
	t.Parallel()

	args := dockerRunArgs("alpine", "echo hi", t.TempDir(), false)

	if slices.Contains(args, "-t") {
		t.Errorf("args = %v, did not want -t", args)
	}
}

func TestDockerRunArgsEmptyCwdMountsNothing(t *testing.T) {
	t.Parallel()

	args := dockerRunArgs("alpine", "echo hi", "", false)

	want := []string{"run", "--rm", "--init", "--", "alpine", "sh", "-c", "echo hi"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

// TestResolveMountPathRejectsColonInPath guards against docker's `-v
// host:container` volume spec silently misparsing a host path that itself
// contains a ':' (a valid POSIX path character) — resolveMountPath (called
// once by NewRunner at construction) must fail loudly instead of letting
// dockerRunArgs build an argument docker would misinterpret.
func TestResolveMountPathRejectsColonInPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cwd := filepath.Join(dir, "weird:name")

	err := os.Mkdir(cwd, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	_, err = resolveMountPath(cwd)
	if err == nil {
		t.Error("expected an error for a working directory containing ':'")
	}
}

// TestNewRunnerRejectsColonInPath confirms the colon rejection surfaces
// through NewRunner (where it's actually triggered in production), not just
// the internal resolveMountPath helper.
func TestNewRunnerRejectsColonInPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cwd := filepath.Join(dir, "weird:name")

	err := os.Mkdir(cwd, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	_, err = NewRunner("alpine", cwd)
	if err == nil {
		t.Error("expected an error for a working directory containing ':'")
	}
}

func TestDockerRunArgsCommandOrdering(t *testing.T) {
	t.Parallel()

	args := dockerRunArgs("myimage", "do the thing", "", false)

	if got := args[len(args)-4:]; !reflect.DeepEqual(got, []string{"myimage", "sh", "-c", "do the thing"}) {
		t.Errorf("tail of args = %v, want [myimage sh -c \"do the thing\"]", got)
	}
}

func TestNewRunner(t *testing.T) {
	t.Parallel()

	hostRunner, err := NewRunner("", "somedir")
	if err != nil {
		t.Fatalf("NewRunner(\"\", ...): %v", err)
	}

	if _, ok := hostRunner.(HostRunner); !ok {
		t.Error("NewRunner(\"\", ...) should return a HostRunner")
	}

	dockerRunnerIface, err := NewRunner("alpine", t.TempDir())
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
// cwd, so Run/RunCapture/RunCaptureFull never need to re-resolve it —
// confirmed here by checking the resolved field directly rather than
// exercising the syscalls indirectly.
func TestNewRunnerResolvesCwdOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	runnerIface, err := NewRunner("alpine", dir)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	runner, ok := runnerIface.(DockerRunner)
	if !ok {
		t.Fatal("expected a DockerRunner")
	}

	want, err := resolveMountPath(dir)
	if err != nil {
		t.Fatal(err)
	}

	if runner.resolvedCwd != want {
		t.Errorf("resolvedCwd = %q, want %q", runner.resolvedCwd, want)
	}
}

// writeFakeDocker installs an executable "docker" script on PATH (via
// t.Setenv, so this test can't run in parallel with siblings) that echoes
// its argv to argvFile and exits with exitCode — a hermetic stand-in so
// DockerRunner's process-plumbing (argv construction, exit code
// propagation, stdout/stderr capture) can be exercised without a real
// docker daemon. stdout/stderr/exitCode are passed to the script via env
// vars (FAKE_DOCKER_*) rather than interpolated into the script's source
// text, so no shell-quoting of arbitrary content is needed at all — os/exec
// passes env values through verbatim, unlike command-line text that a
// shell re-parses.
func writeFakeDocker(t *testing.T, exitCode int, stdout, stderr string) (argvFile string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("fake docker script assumes a POSIX shell")
	}

	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv.txt")

	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" > " + argvFile + "\n" +
		"printf '%s' \"$FAKE_DOCKER_STDOUT\"\n" +
		"printf '%s' \"$FAKE_DOCKER_STDERR\" >&2\n" +
		"exit \"$FAKE_DOCKER_EXIT\"\n"

	scriptPath := filepath.Join(dir, "docker")

	err := os.WriteFile(scriptPath, []byte(script), 0o700) //nolint:gosec // test fixture, needs to be executable
	if err != nil {
		t.Fatalf("write fake docker: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_STDOUT", stdout)
	t.Setenv("FAKE_DOCKER_STDERR", stderr)
	t.Setenv("FAKE_DOCKER_EXIT", strconv.Itoa(exitCode))

	return argvFile
}

func TestDockerRunnerCaptureFull(t *testing.T) {
	argvFile := writeFakeDocker(t, 3, "out text", "err text")

	runner, err := NewRunner("alpine", t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	stdout, stderr, exitCode, err := runner.RunCaptureFull(context.Background(), "do stuff")
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

	recorded, readErr := os.ReadFile(argvFile) //nolint:gosec // test fixture path
	if readErr != nil {
		t.Fatalf("read recorded argv: %v", readErr)
	}

	fields := strings.Fields(string(recorded))
	if !strings.Contains(string(recorded), "do stuff") || !strings.Contains(string(recorded), "alpine") || slices.Contains(fields, "-i") {
		t.Errorf("recorded argv = %q, want it to contain the image/command and omit the -i token (RunCaptureFull is non-interactive)", recorded)
	}
}

// TestDockerRunnerCaptureFullHandlesAdversarialOutput guards writeFakeDocker
// itself: stdout/stderr containing shell metacharacters (single quotes,
// backslashes, a trailing unescaped quote) must round-trip byte-for-byte,
// since they're passed through env vars rather than interpolated into the
// fake script's source text.
func TestDockerRunnerCaptureFullHandlesAdversarialOutput(t *testing.T) {
	const adversarialStdout = `it's a "test" with \backslashes\ and 'quotes'`

	writeFakeDocker(t, 0, adversarialStdout, "")

	runner, err := NewRunner("alpine", t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	stdout, _, _, err := runner.RunCaptureFull(context.Background(), "do stuff")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	if stdout != adversarialStdout {
		t.Errorf("stdout = %q, want %q", stdout, adversarialStdout)
	}
}

func TestDockerRunnerRunErrorsOnNonzeroExit(t *testing.T) {
	writeFakeDocker(t, 1, "", "boom")

	runner, err := NewRunner("alpine", t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	err = runner.Run(context.Background(), "false")
	if err == nil {
		t.Error("expected an error for a nonzero exit from Run (unlike RunCaptureFull)")
	}
}

func TestDockerRunnerRunCaptureReturnsStdout(t *testing.T) {
	writeFakeDocker(t, 0, "captured output", "")

	runner, err := NewRunner("alpine", t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	out, err := runner.RunCapture(context.Background(), "echo hi")
	if err != nil {
		t.Fatalf("RunCapture: %v", err)
	}

	if string(out) != "captured output" {
		t.Errorf("out = %q, want %q", out, "captured output")
	}
}

// TestDockerRunnerRunCaptureLogsStderrOnFailure guards against a regression
// where DockerRunner.RunCapture streamed stderr live but never captured it
// for the debug log, unlike HostRunner.RunCapture (whose doc comment
// explains this exists so a failing check/out command's output is available
// for debugging, not just discarded on nonzero exit).
func TestDockerRunnerRunCaptureLogsStderrOnFailure(t *testing.T) {
	writeFakeDocker(t, 1, "", "boom from stderr")

	runner, err := NewRunner("alpine", t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	var logBuf bytes.Buffer

	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	defer slog.SetDefault(prevLogger)

	_, err = runner.RunCapture(context.Background(), "false")
	if err == nil {
		t.Fatal("expected an error for a nonzero exit")
	}

	if !strings.Contains(logBuf.String(), "boom from stderr") {
		t.Errorf("debug log = %q, want it to contain the captured stderr", logBuf.String())
	}
}

func TestValidateDockerMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty PATH: docker cannot be found

	err := ValidateDocker(context.Background())
	if err == nil {
		t.Error("expected an error when docker is not on PATH")
	}
}
