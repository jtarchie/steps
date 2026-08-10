package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// TestDockerStartArgsMountsCwd and its siblings below each assert one facet
// of the argv construction behind a step's container; split into separate
// top-level functions (rather than t.Run subtests of one function) to stay
// under the linter's per-function cyclomatic-complexity budget.
// dockerStartArgs takes an already-resolved cwd (resolution happens once in
// NewRunner, not here — see resolveMountPath) so these tests pass a plain
// absolute path directly rather than exercising resolution.
func TestDockerStartArgsMountsCwd(t *testing.T) {
	t.Parallel()

	resolvedCwd := t.TempDir()

	args := dockerStartArgs("alpine", "steps-abc", resolvedCwd, nil, "", "", false, 0, 0)

	if !slices.Contains(args, "-v") {
		t.Fatalf("args = %v, want a -v mount", args)
	}

	i := slices.Index(args, "-v")
	if args[i+1] != resolvedCwd+":"+resolvedCwd {
		t.Errorf("mount = %q, want the cwd bound at its own path", args[i+1])
	}

	w := slices.Index(args, "-w")
	if w < 0 || args[w+1] != resolvedCwd {
		t.Errorf("args = %v, want -w %s", args, resolvedCwd)
	}

	if got := args[len(args)-4:]; !reflect.DeepEqual(got, []string{"alpine", "sh", "-c", "sleep 86400"}) {
		t.Errorf("tail of args = %v, want the image and keepalive last", got)
	}
}

func TestDockerStartArgsEmptyCwdMountsNothing(t *testing.T) {
	t.Parallel()

	args := dockerStartArgs("alpine", "steps-abc", "", nil, "", "", false, 0, 0)

	if slices.Contains(args, "-v") || slices.Contains(args, "-w") {
		t.Errorf("args = %v, want no mount or workdir for an empty cwd", args)
	}
}

// TestDockerStartArgsImageIsPositional guards the "--" that stops docker's
// flag parser from reading an image value as a flag of its own — see
// dockerStartArgs' doc and config.validateImageValues.
func TestDockerStartArgsImageIsPositional(t *testing.T) {
	t.Parallel()

	args := dockerStartArgs("--privileged", "steps-abc", "", nil, "", "", false, 0, 0)

	sep := slices.Index(args, "--")
	if sep < 0 {
		t.Fatalf("args = %v, want a -- separator before the image", args)
	}

	if args[sep+1] != "--privileged" {
		t.Errorf("args[%d] = %q, want the image to sit immediately after --", sep+1, args[sep+1])
	}
}

// TestDockerStartArgsKeepaliveIsBounded guards what keeps a SIGKILLed steps
// process from stranding a RUNNING container forever: the keepalive must
// terminate on its own, leaving at worst an inert Exited row for the next
// run's sweep. An endless loop would reintroduce exactly the orphan the
// session was built to eliminate.
//
// --rm must NOT be present: a self-removing container takes its own exit code
// and logs with it, and those are the entire diagnosis when an image rejects
// the keepalive (see checkAlive).
func TestDockerStartArgsKeepaliveIsBounded(t *testing.T) {
	t.Parallel()

	args := dockerStartArgs("alpine", "steps-abc", "", nil, "", "", false, 0, 0)

	if slices.Contains(args, "--rm") {
		t.Errorf("args = %v, did not want --rm — it would destroy the postmortem a failed start needs", args)
	}

	keepalive := args[len(args)-1]
	if !strings.HasPrefix(keepalive, "sleep ") {
		t.Errorf("keepalive = %q, want a bounded sleep", keepalive)
	}

	secs, err := strconv.Atoi(strings.TrimPrefix(keepalive, "sleep "))
	if err != nil {
		t.Fatalf("keepalive %q does not end in a number: %v", keepalive, err)
	}

	if secs != int(dockerSessionLifetime.Seconds()) {
		t.Errorf("keepalive = %ds, want %ds", secs, int(dockerSessionLifetime.Seconds()))
	}
}

func TestDockerExecArgsStdin(t *testing.T) {
	t.Parallel()

	args := dockerExecArgs("steps-abc", "echo hi", true)

	want := []string{"exec", "-i", "--", "steps-abc", "sh", "-c", "echo hi"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestDockerExecArgsStdinFalseOmitsDashI(t *testing.T) {
	t.Parallel()

	args := dockerExecArgs("steps-abc", "echo hi", false)

	if slices.Contains(args, "-i") {
		t.Errorf("args = %v, did not want -i", args)
	}
}

// TestDockerNeverPassesDashT covers both argv builders at once: a -t would
// allocate a TTY, which changes how output is framed and is never wanted for
// a non-interactive pipeline command.
func TestDockerNeverPassesDashT(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		dockerStartArgs("alpine", "steps-abc", t.TempDir(), nil, "", "", false, 0, 0),
		dockerExecArgs("steps-abc", "echo hi", true),
		dockerExecArgs("steps-abc", "echo hi", false),
	} {
		if slices.Contains(args, "-t") {
			t.Errorf("args = %v, did not want -t", args)
		}
	}
}

func TestDockerExecArgsCommandOrdering(t *testing.T) {
	t.Parallel()

	args := dockerExecArgs("steps-abc", "do the thing", false)

	if got := args[len(args)-4:]; !reflect.DeepEqual(got, []string{"steps-abc", "sh", "-c", "do the thing"}) {
		t.Errorf("tail of args = %v, want [steps-abc sh -c \"do the thing\"]", got)
	}
}

// TestNewContainerNameIsUnique guards the reason names are random rather than
// derived from a step's name: two concurrent runs of the same step must never
// contend for one container.
func TestNewContainerNameIsUnique(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}

	for range 100 {
		name, err := newContainerName()
		if err != nil {
			t.Fatalf("newContainerName: %v", err)
		}

		if !strings.HasPrefix(name, "steps-") {
			t.Errorf("name = %q, want a steps- prefix", name)
		}

		if seen[name] {
			t.Fatalf("newContainerName returned %q twice", name)
		}

		seen[name] = true
	}
}

// TestResolveMountPathRejectsColonInPath guards against docker's `-v
// host:container` volume spec silently misparsing a host path that itself
// contains a ':' (a valid POSIX path character) — resolveMountPath (called
// once by NewRunner at construction) must fail loudly instead of letting
// dockerStartArgs build an argument docker would misinterpret.
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

// TestNewRunnerDoesNotStartAContainer pins the laziness: constructing a
// runner must cost nothing. A step whose command is skipped, or which fails
// before running anything, should never have paid for a container.
func TestNewRunnerDoesNotStartAContainer(t *testing.T) {
	argvFile := writeFakeDocker(t, 0, "", "")

	_, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	_, statErr := os.Stat(argvFile)
	if statErr == nil {
		t.Error("NewRunner invoked docker; it should not start a container until the first command")
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

	runnerIface, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: dir})
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

	if runner.session.resolvedCwd != want {
		t.Errorf("resolvedCwd = %q, want %q", runner.session.resolvedCwd, want)
	}
}

// writeFakeDocker installs an executable "docker" script on PATH (via
// t.Setenv, so this test can't run in parallel with siblings) that appends
// its argv to argvFile and exits with exitCode — a hermetic stand-in so
// DockerRunner's process-plumbing (argv construction, exit code
// propagation, stdout/stderr capture) can be exercised without a real
// docker daemon. stdout/stderr/exitCode are passed to the script via env
// vars (FAKE_DOCKER_*) rather than interpolated into the script's source
// text, so no shell-quoting of arbitrary content is needed at all — os/exec
// passes env values through verbatim, unlike command-line text that a
// shell re-parses.
//
// The configured exit code and output apply only to `docker exec` — the
// command the caller actually asked to run. Session lifecycle invocations
// (`run -d` to start the container, `rm -f` to remove it) succeed on their
// own, so a test asserting on a command's failure isn't really asserting on
// a failure to start the container. FAKE_DOCKER_RUN_EXIT overrides that for
// the tests that want the start itself to fail.
func writeFakeDocker(t *testing.T, exitCode int, stdout, stderr string) (argvFile string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("fake docker script assumes a POSIX shell")
	}

	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv.txt")

	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + argvFile + "\n" +
		"case \"$1\" in\n" +
		"  run)     printf 'fakecontainerid\\n'; printf '%s' \"$FAKE_DOCKER_RUN_STDERR\" >&2; exit \"${FAKE_DOCKER_RUN_EXIT:-0}\" ;;\n" +
		"  inspect) printf '%s\\n' \"${FAKE_DOCKER_STATE:-true 0}\"; exit 0 ;;\n" +
		"  logs)    printf '%s\\n' \"$FAKE_DOCKER_LOGS\"; exit 0 ;;\n" +
		"  rm)      exit 0 ;;\n" +
		"esac\n" +
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

// recordedArgv returns each docker invocation the fake recorded, in order.
func recordedArgv(t *testing.T, argvFile string) []string {
	t.Helper()

	recorded, err := os.ReadFile(argvFile) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}

	return strings.Split(strings.TrimRight(string(recorded), "\n"), "\n")
}

// invocationsOf returns the recorded invocations whose docker subcommand is
// verb (e.g. "run", "exec", "rm").
func invocationsOf(lines []string, verb string) []string {
	var out []string

	for _, line := range lines {
		if strings.HasPrefix(line, verb+" ") {
			out = append(out, line)
		}
	}

	return out
}

func TestDockerRunnerCaptureFull(t *testing.T) {
	writeFakeDocker(t, 3, "out text", "err text")

	runner, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

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
}

// TestDockerRunnerCaptureFullArgv is TestDockerRunnerCaptureFull's argv half,
// split off to stay under the linter's per-function complexity budget.
func TestDockerRunnerCaptureFullArgv(t *testing.T) {
	argvFile := writeFakeDocker(t, 0, "", "")

	runner, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	_, _, _, err = runner.RunCaptureFull(context.Background(), "do stuff")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	lines := recordedArgv(t, argvFile)

	starts := invocationsOf(lines, "run")
	if len(starts) != 1 || !strings.Contains(starts[0], "alpine") {
		t.Errorf("run invocations = %v, want exactly one naming the image", starts)
	}

	execs := invocationsOf(lines, "exec")
	if len(execs) != 1 {
		t.Fatalf("exec invocations = %v, want exactly one", execs)
	}

	if !strings.Contains(execs[0], "do stuff") || slices.Contains(strings.Fields(execs[0]), "-i") {
		t.Errorf("exec argv = %q, want it to contain the command and omit the -i token (RunCaptureFull is non-interactive)", execs[0])
	}
}

// TestDockerRunnerReusesOneContainer is the point of the whole session: two
// commands from one runner must land in the SAME container, so state a model
// established in one run_shell call (an installed package, an exported
// variable, a created file outside the mount) is still there for the next.
func TestDockerRunnerReusesOneContainer(t *testing.T) {
	argvFile := writeFakeDocker(t, 0, "", "")

	runner, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	for _, command := range []string{"first", "second", "third"} {
		_, _, _, runErr := runner.RunCaptureFull(context.Background(), command)
		if runErr != nil {
			t.Fatalf("RunCaptureFull(%q): %v", command, runErr)
		}
	}

	lines := recordedArgv(t, argvFile)

	if starts := invocationsOf(lines, "run"); len(starts) != 1 {
		t.Errorf("run invocations = %v, want exactly one for three commands", starts)
	}

	execs := invocationsOf(lines, "exec")
	if len(execs) != 3 {
		t.Fatalf("exec invocations = %v, want three", execs)
	}

	name := containerNameFromExec(t, execs[0])
	for _, exec := range execs[1:] {
		if got := containerNameFromExec(t, exec); got != name {
			t.Errorf("exec targeted container %q, want %q — every command must share one container", got, name)
		}
	}
}

// containerNameFromExec pulls the container name out of a recorded
// `exec [-i] -- <name> sh -c ...` invocation.
func containerNameFromExec(t *testing.T, line string) string {
	t.Helper()

	fields := strings.Fields(line)

	sep := slices.Index(fields, "--")
	if sep < 0 || sep+1 >= len(fields) {
		t.Fatalf("exec argv %q has no -- separator followed by a container name", line)
	}

	return fields[sep+1]
}

// TestDockerRunnerCloseRemovesContainer guards the orphan fix: whatever
// happens to an individual exec client, Close names the container and forces
// it away.
func TestDockerRunnerCloseRemovesContainer(t *testing.T) {
	argvFile := writeFakeDocker(t, 0, "", "")

	runner, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	_, _, _, err = runner.RunCaptureFull(context.Background(), "anything")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	err = runner.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := recordedArgv(t, argvFile)

	removes := invocationsOf(lines, "rm")
	if len(removes) != 1 {
		t.Fatalf("rm invocations = %v, want exactly one", removes)
	}

	name := containerNameFromExec(t, invocationsOf(lines, "exec")[0])
	if !strings.Contains(removes[0], name) {
		t.Errorf("rm argv = %q, want it to name the container %q", removes[0], name)
	}

	if !strings.Contains(removes[0], "-f") {
		t.Errorf("rm argv = %q, want -f so a still-running command cannot block teardown", removes[0])
	}
}

// TestDockerRunnerCloseWithoutCommandsRemovesNothing pairs with
// TestNewRunnerDoesNotStartAContainer: a runner that never ran anything has
// no container, and Close must not invent a docker call (or an error) for it.
func TestDockerRunnerCloseWithoutCommandsRemovesNothing(t *testing.T) {
	argvFile := writeFakeDocker(t, 0, "", "")

	runner, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	err = runner.Close()
	if err != nil {
		t.Fatalf("Close on an unused runner: %v", err)
	}

	_, statErr := os.Stat(argvFile)
	if statErr == nil {
		t.Errorf("Close invoked docker for a runner that never started a container: %v", recordedArgv(t, argvFile))
	}
}

// TestDockerRunnerCloseIsIdempotent covers the double-close that falls out of
// real wiring: an agent step registers its runner in closers AND a caller may
// defer CloseRunner around it.
func TestDockerRunnerCloseIsIdempotent(t *testing.T) {
	argvFile := writeFakeDocker(t, 0, "", "")

	runner, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	_, _, _, err = runner.RunCaptureFull(context.Background(), "anything")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	for i := range 3 {
		closeErr := runner.Close()
		if closeErr != nil {
			t.Fatalf("Close #%d: %v", i+1, closeErr)
		}
	}

	if removes := invocationsOf(recordedArgv(t, argvFile), "rm"); len(removes) != 1 {
		t.Errorf("rm invocations = %v, want exactly one across three Close calls", removes)
	}
}

// TestDockerRunnerWithLabelSharesOneContainer guards the aliasing WithLabel
// introduces: it returns a copy, and a copy that started its own container
// would silently double a step's containers and split its state in half.
func TestDockerRunnerWithLabelSharesOneContainer(t *testing.T) {
	argvFile := writeFakeDocker(t, 0, "", "")

	runner, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	labeled := runner.WithLabel("agent")

	_, _, _, err = runner.RunCaptureFull(context.Background(), "from the original")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	_, _, _, err = labeled.RunCaptureFull(context.Background(), "from the copy")
	if err != nil {
		t.Fatalf("RunCaptureFull (labeled): %v", err)
	}

	if starts := invocationsOf(recordedArgv(t, argvFile), "run"); len(starts) != 1 {
		t.Errorf("run invocations = %v, want exactly one shared by the runner and its labeled copy", starts)
	}
}

// TestDockerRunnerStartFailureSurfacesAsData pins the semantics documented in
// docs/infra.md, which the move to a session had to preserve: a docker-level
// failure (an unknown image exits 125) reaches an agent as an ordinary tool
// result — an exit code and stderr — not as a Go error that aborts the step.
func TestDockerRunnerStartFailureSurfacesAsData(t *testing.T) {
	writeFakeDocker(t, 0, "", "")
	t.Setenv("FAKE_DOCKER_RUN_EXIT", "125")
	t.Setenv("FAKE_DOCKER_RUN_STDERR", "Unable to find image 'nope:latest'")

	runner, err := NewRunner(RunnerSpec{Image: "nope", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	_, stderr, exitCode, err := runner.RunCaptureFull(context.Background(), "anything")
	if err != nil {
		t.Fatalf("RunCaptureFull returned a Go error for a docker-level failure: %v", err)
	}

	if exitCode != 125 {
		t.Errorf("exitCode = %d, want 125 from the failed container start", exitCode)
	}

	if !strings.Contains(stderr, "Unable to find image") {
		t.Errorf("stderr = %q, want docker's own start failure", stderr)
	}
}

// TestDockerRunnerStartFailureIsSticky guards against re-paying for a
// hopeless start on every command: an agent conversation with a bad image
// would otherwise attempt (and, for a real daemon, re-pull) once per
// run_shell call.
func TestDockerRunnerStartFailureIsSticky(t *testing.T) {
	argvFile := writeFakeDocker(t, 0, "", "")
	t.Setenv("FAKE_DOCKER_RUN_EXIT", "125")

	runner, err := NewRunner(RunnerSpec{Image: "nope", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	for range 3 {
		_, _, exitCode, runErr := runner.RunCaptureFull(context.Background(), "anything")
		if runErr != nil {
			t.Fatalf("RunCaptureFull: %v", runErr)
		}

		if exitCode != 125 {
			t.Errorf("exitCode = %d, want 125 on every command once the start failed", exitCode)
		}
	}

	lines := recordedArgv(t, argvFile)

	if starts := invocationsOf(lines, "run"); len(starts) != 1 {
		t.Errorf("run invocations = %v, want exactly one — a failed start must not be retried per command", starts)
	}

	if execs := invocationsOf(lines, "exec"); len(execs) != 0 {
		t.Errorf("exec invocations = %v, want none against a container that never started", execs)
	}
}

// TestDockerRunnerStartFailureIsAnExitError keeps a containerized task's
// docker-level failure classifying the way it always has: runTaskCommand asks
// shell.IsExitError to tell a task failure from an infrastructure error, and
// a start failure has to answer that question the same as any nonzero exit.
func TestDockerRunnerStartFailureIsAnExitError(t *testing.T) {
	writeFakeDocker(t, 0, "", "")
	t.Setenv("FAKE_DOCKER_RUN_EXIT", "125")

	runner, err := NewRunner(RunnerSpec{Image: "nope", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	err = runner.Run(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected an error when the container could not start")
	}

	if !IsExitError(err) {
		t.Errorf("IsExitError(%v) = false, want true so a containerized task still classifies as failed", err)
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

	runner, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

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

	runner, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	err = runner.Run(context.Background(), "false")
	if err == nil {
		t.Error("expected an error for a nonzero exit from Run (unlike RunCaptureFull)")
	}
}

func TestDockerRunnerRunCaptureReturnsStdout(t *testing.T) {
	writeFakeDocker(t, 0, "captured output", "")

	runner, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

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

	runner, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

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

// TestDockerStartArgsPassesEnvByNameNotValue is the security-relevant half of
// env: on the container path. `-e NAME=value` would put the secret in the
// docker client's argv, readable by anything that can list host processes for
// as long as the command runs — strictly worse than the host-side exposure
// env: exists to manage. `-e NAME` makes the docker CLI forward the value out
// of its own environment instead, so it never appears in an argument.
func TestDockerStartArgsPassesEnvByNameNotValue(t *testing.T) {
	t.Setenv("STEPS_TEST_SECRET", "hunter2")

	args := dockerStartArgs("alpine", "steps-abc", "", []string{"STEPS_TEST_SECRET"}, "", "", false, 0, 0)

	i := slices.Index(args, "-e")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("args = %v, want an -e flag", args)
	}

	if args[i+1] != "STEPS_TEST_SECRET" {
		t.Errorf("args[%d] = %q, want the bare variable name", i+1, args[i+1])
	}

	for _, arg := range args {
		if strings.Contains(arg, "hunter2") {
			t.Errorf("args = %v, want no argument to carry the variable's VALUE", args)
		}
	}
}

func TestDockerStartArgsNoEnvAddsNoFlags(t *testing.T) {
	t.Parallel()

	if args := dockerStartArgs("alpine", "steps-abc", "", nil, "", "", false, 0, 0); slices.Contains(args, "-e") {
		t.Errorf("args = %v, did not want an -e flag when env: is unset", args)
	}
}

// TestDockerStartArgsEnvPrecedesTheImage guards placement: an -e appended after
// the "--" separator would be read as an argument to the container's command
// rather than a flag to docker run.
func TestDockerStartArgsEnvPrecedesTheImage(t *testing.T) {
	t.Parallel()

	args := dockerStartArgs("alpine", "steps-abc", "", []string{"A", "B"}, "", "", false, 0, 0)

	sep := slices.Index(args, "--")
	if sep < 0 {
		t.Fatalf("args = %v, want a -- separator", args)
	}

	if last := slices.Index(args[sep:], "-e"); last >= 0 {
		t.Errorf("args = %v, want every -e before the -- separator", args)
	}
}

// TestDockerStartArgsUserIsPassedThrough covers an explicit user:.
func TestDockerStartArgsUserIsPassedThrough(t *testing.T) {
	t.Parallel()

	args := dockerStartArgs("alpine", "steps-abc", "", nil, "root", "", false, 0, 0)

	i := slices.Index(args, "--user")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("args = %v, want a --user flag", args)
	}

	if args[i+1] != "root" {
		t.Errorf("args[%d] = %q, want root", i+1, args[i+1])
	}
}

func TestDockerStartArgsNoUserAddsNoFlag(t *testing.T) {
	t.Parallel()

	if args := dockerStartArgs("alpine", "steps-abc", "", nil, "", "", false, 0, 0); slices.Contains(args, "--user") {
		t.Errorf("args = %v, did not want a --user flag when none was resolved", args)
	}
}

// TestContainerUserPrefersTheConfiguredValue pins that user: always wins over
// the platform default — it is the documented escape hatch for an image that
// needs root, so a platform default silently overriding it would make the
// knob useless exactly where it matters.
func TestContainerUserPrefersTheConfiguredValue(t *testing.T) {
	t.Parallel()

	if got := containerUser("root"); got != "root" {
		t.Errorf("containerUser(\"root\") = %q, want root", got)
	}

	if got := containerUser("1234:5678"); got != "1234:5678" {
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

// TestDockerStartArgsNetworkIsPassedThrough covers the sandboxing knob: an
// agent's model-directed run_shell had unrestricted egress with no way to say
// otherwise, and `network: none` is the answer.
func TestDockerStartArgsNetworkIsPassedThrough(t *testing.T) {
	t.Parallel()

	args := dockerStartArgs("alpine", "steps-abc", "", nil, "", "none", false, 0, 0)

	i := slices.Index(args, "--network")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("args = %v, want a --network flag", args)
	}

	if args[i+1] != "none" {
		t.Errorf("args[%d] = %q, want none", i+1, args[i+1])
	}

	sep := slices.Index(args, "--")
	if i > sep {
		t.Errorf("args = %v, want --network before the -- separator", args)
	}
}

func TestDockerStartArgsNoNetworkAddsNoFlag(t *testing.T) {
	t.Parallel()

	if args := dockerStartArgs("alpine", "steps-abc", "", nil, "", "", false, 0, 0); slices.Contains(args, "--network") {
		t.Errorf("args = %v, did not want a --network flag when none was set", args)
	}
}

// TestDockerRunnerDetectsAContainerThatDiedAtBirth is the case `docker run -d`
// cannot report: it exits 0 for a container that stopped a millisecond later,
// which is the NORMAL outcome for an image with an ENTRYPOINT, since the
// keepalive becomes arguments to that entrypoint instead of replacing it.
// Trusting exit 0 left every later exec saying "No such container", which
// names neither the image nor the reason.
func TestDockerRunnerDetectsAContainerThatDiedAtBirth(t *testing.T) {
	writeFakeDocker(t, 0, "", "")
	t.Setenv("FAKE_DOCKER_STATE", "false 127")
	t.Setenv("FAKE_DOCKER_LOGS", "git: 'sh' is not a git command")

	runner, err := NewRunner(RunnerSpec{Image: "alpine/git", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer CloseRunner(runner, "test")

	_, _, _, err = runner.RunCaptureFull(t.Context(), "anything")
	if err == nil {
		t.Fatal("expected an error when the container did not stay up")
	}

	for _, want := range []string{"alpine/git", "127", "not a git command"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

// TestDockerRunnerRemovesAContainerThatDiedAtBirth pins that diagnosing the
// corpse does not mean keeping it: --rm is gone precisely so the postmortem
// survives long enough to read, which makes removing it afterwards our job.
func TestDockerRunnerRemovesAContainerThatDiedAtBirth(t *testing.T) {
	argvFile := writeFakeDocker(t, 0, "", "")
	t.Setenv("FAKE_DOCKER_STATE", "false 1")

	runner, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	_, _, _, _ = runner.RunCaptureFull(t.Context(), "anything")

	if removes := invocationsOf(recordedArgv(t, argvFile), "rm"); len(removes) != 1 {
		t.Errorf("rm invocations = %v, want exactly one for the dead container", removes)
	}
}

// TestDockerRunnerRejectsCommandsAfterClose guards the aliasing hazard Close
// creates: it clears the container name, so without an explicit closed flag
// the next command would exec against an empty name and report a malformed
// docker invocation as an ordinary exit code.
func TestDockerRunnerRejectsCommandsAfterClose(t *testing.T) {
	argvFile := writeFakeDocker(t, 0, "", "")

	runner, err := NewRunner(RunnerSpec{Image: "alpine", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	_, _, _, err = runner.RunCaptureFull(t.Context(), "first")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	err = runner.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, _, _, err = runner.RunCaptureFull(t.Context(), "after close")
	if !errors.Is(err, errSessionClosed) {
		t.Errorf("err = %v, want errSessionClosed", err)
	}

	for _, line := range invocationsOf(recordedArgv(t, argvFile), "exec") {
		if strings.Contains(line, "after close") {
			t.Errorf("a command ran after Close: %q", line)
		}
	}
}

// TestDockerRunnerCloseToleratesAnAlreadyGoneContainer keeps a clean teardown
// from logging an error: a container that exited on its own (the keepalive
// expiring, a daemon restart) is the outcome Close wanted.
func TestDockerRunnerCloseToleratesAnAlreadyGoneContainer(t *testing.T) {
	if !containerAlreadyGone([]byte("Error response from daemon: No such container: steps-abc")) {
		t.Error("containerAlreadyGone did not recognize docker's own wording")
	}

	if containerAlreadyGone([]byte("Error response from daemon: permission denied")) {
		t.Error("containerAlreadyGone treated an unrelated failure as success")
	}
}

// TestDockerStartArgsPrivilegedAndLimits pins the three flags container
// isolation is actually configured with, and — more importantly — that each is
// OMITTED rather than passed as a zero when unset.
//
// Passing --cpu-shares 0 would mean the same thing to docker as omitting it,
// but it would show up in `docker inspect` as a configured limit of zero,
// which reads as a misconfiguration rather than as "no limit asked for".
func TestDockerStartArgsPrivilegedAndLimits(t *testing.T) {
	t.Parallel()

	args := dockerStartArgs("alpine", "steps-abc", "", nil, "", "", true, 512, 1<<30)

	if !slices.Contains(args, "--privileged") {
		t.Errorf("args = %v, want --privileged", args)
	}

	for flag, want := range map[string]string{
		"--cpu-shares": "512",
		"--memory":     "1073741824",
	} {
		i := slices.Index(args, flag)
		if i < 0 {
			t.Errorf("args = %v, want %s", args, flag)

			continue
		}

		if args[i+1] != want {
			t.Errorf("%s = %q, want %q", flag, args[i+1], want)
		}
	}

	// Unset: every one of them absent, not present-and-zero.
	bare := dockerStartArgs("alpine", "steps-abc", "", nil, "", "", false, 0, 0)

	for _, flag := range []string{"--privileged", "--cpu-shares", "--memory"} {
		if slices.Contains(bare, flag) {
			t.Errorf("bare args = %v, want no %s when unset", bare, flag)
		}
	}
}
