package shell

import (
	"context"
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
// per-function cyclomatic-complexity budget.
func TestDockerRunArgsMountsCwd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	args, resolvedCwd, err := dockerRunArgs("alpine", "echo hi", dir, true)
	if err != nil {
		t.Fatalf("dockerRunArgs: %v", err)
	}

	want := []string{"run", "--rm", "--init", "-i", "-v", resolvedCwd + ":" + resolvedCwd, "-w", resolvedCwd, "alpine", "sh", "-c", "echo hi"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestDockerRunArgsStdinFalseOmitsDashI(t *testing.T) {
	t.Parallel()

	args, _, err := dockerRunArgs("alpine", "echo hi", t.TempDir(), false)
	if err != nil {
		t.Fatalf("dockerRunArgs: %v", err)
	}

	if slices.Contains(args, "-i") {
		t.Errorf("args = %v, did not want -i", args)
	}
}

func TestDockerRunArgsNeverPassesDashTWithStdin(t *testing.T) {
	t.Parallel()

	args, _, err := dockerRunArgs("alpine", "echo hi", t.TempDir(), true)
	if err != nil {
		t.Fatalf("dockerRunArgs: %v", err)
	}

	if slices.Contains(args, "-t") {
		t.Errorf("args = %v, did not want -t", args)
	}
}

func TestDockerRunArgsNeverPassesDashTWithoutStdin(t *testing.T) {
	t.Parallel()

	args, _, err := dockerRunArgs("alpine", "echo hi", t.TempDir(), false)
	if err != nil {
		t.Fatalf("dockerRunArgs: %v", err)
	}

	if slices.Contains(args, "-t") {
		t.Errorf("args = %v, did not want -t", args)
	}
}

func TestDockerRunArgsEmptyCwdMountsNothing(t *testing.T) {
	t.Parallel()

	args, resolvedCwd, err := dockerRunArgs("alpine", "echo hi", "", false)
	if err != nil {
		t.Fatalf("dockerRunArgs: %v", err)
	}

	if resolvedCwd != "" {
		t.Errorf("resolvedCwd = %q, want empty", resolvedCwd)
	}

	want := []string{"run", "--rm", "--init", "alpine", "sh", "-c", "echo hi"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestDockerRunArgsCommandOrdering(t *testing.T) {
	t.Parallel()

	args, _, err := dockerRunArgs("myimage", "do the thing", "", false)
	if err != nil {
		t.Fatalf("dockerRunArgs: %v", err)
	}

	if got := args[len(args)-4:]; !reflect.DeepEqual(got, []string{"myimage", "sh", "-c", "do the thing"}) {
		t.Errorf("tail of args = %v, want [myimage sh -c \"do the thing\"]", got)
	}
}

func TestNewRunner(t *testing.T) {
	t.Parallel()

	if _, ok := NewRunner("").(HostRunner); !ok {
		t.Error("NewRunner(\"\") should return a HostRunner")
	}

	runner, ok := NewRunner("alpine").(DockerRunner)
	if !ok {
		t.Fatal("NewRunner(\"alpine\") should return a DockerRunner")
	}

	if runner.Image != "alpine" {
		t.Errorf("Image = %q, want alpine", runner.Image)
	}
}

// writeFakeDocker installs an executable "docker" script on PATH (via
// t.Setenv, so this test can't run in parallel with siblings) that echoes
// its argv to argvFile and exits with exitCode — a hermetic stand-in so
// DockerRunner's process-plumbing (argv construction, exit code
// propagation, stdout/stderr capture) can be exercised without a real
// docker daemon.
func writeFakeDocker(t *testing.T, exitCode int, stdout, stderr string) (argvFile string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("fake docker script assumes a POSIX shell")
	}

	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv.txt")

	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" > " + argvFile + "\n" +
		"printf '%s' " + shellSingleQuote(stdout) + "\n" +
		"printf '%s' " + shellSingleQuote(stderr) + " >&2\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"

	scriptPath := filepath.Join(dir, "docker")

	err := os.WriteFile(scriptPath, []byte(script), 0o700) //nolint:gosec // test fixture, needs to be executable
	if err != nil {
		t.Fatalf("write fake docker: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return argvFile
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func TestDockerRunnerCaptureFull(t *testing.T) {
	argvFile := writeFakeDocker(t, 3, "out text", "err text")

	stdout, stderr, exitCode, err := DockerRunner{Image: "alpine"}.RunCaptureFull(context.Background(), "do stuff", t.TempDir())
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

func TestDockerRunnerRunErrorsOnNonzeroExit(t *testing.T) {
	writeFakeDocker(t, 1, "", "boom")

	err := DockerRunner{Image: "alpine"}.Run(context.Background(), "false", t.TempDir())
	if err == nil {
		t.Error("expected an error for a nonzero exit from Run (unlike RunCaptureFull)")
	}
}

func TestDockerRunnerRunCaptureReturnsStdout(t *testing.T) {
	writeFakeDocker(t, 0, "captured output", "")

	out, err := DockerRunner{Image: "alpine"}.RunCapture(context.Background(), "echo hi", t.TempDir())
	if err != nil {
		t.Fatalf("RunCapture: %v", err)
	}

	if string(out) != "captured output" {
		t.Errorf("out = %q, want %q", out, "captured output")
	}
}

func TestValidateDockerMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty PATH: docker cannot be found

	err := ValidateDocker(context.Background())
	if err == nil {
		t.Error("expected an error when docker is not on PATH")
	}
}
