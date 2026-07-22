package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// dockerKillGrace bounds how long a canceled docker CLI client is given to
// exit cleanly (SIGTERM, sig-proxied into the container) before Go force-
// kills it. A container can outlive a force-killed client until the daemon
// notices; this is an accepted tradeoff for v1 rather than tracking
// container names for an explicit `docker kill`.
const dockerKillGrace = 10 * time.Second

// DockerRunner runs commands inside a fresh `docker run --rm` container per
// call, all against resolvedCwd — resolved and validated once by NewRunner
// at construction, not re-resolved on every call. The working directory is
// bind-mounted at its own (resolved) host path and set as the container's
// workdir, so host-side readers (agent read_file/list_dir, workspace
// Capture) see the same files a containerized command wrote. No host
// environment variables are passed through — the container starts from the
// image's own env only.
type DockerRunner struct {
	Image       string
	resolvedCwd string
}

// dockerExec is the shared plumbing behind Run/RunCapture/RunCaptureFull/
// RunCaptureFullLimited: it always captures both stdout and stderr — even
// for Run, which never exposes them, trading a little memory for one less
// code path to keep in sync across four near-identical methods —
// additionally streaming either live (to os.Stdout/os.Stderr) when
// streamStdout/streamStderr is set. stdin wires the host's stdin through
// only when set; otherwise cmd.Stdin stays nil (/dev/null), matching
// RunCaptureFull's non-interactive semantics for model-generated commands.
// maxBytes caps each captured stream (0 means unbounded — every caller but
// RunCaptureFullLimited passes 0, reproducing today's behavior exactly).
func (d DockerRunner) dockerExec(
	ctx context.Context, command string, stdin, streamStdout, streamStderr bool, maxBytes int,
) (stdout, stderr string, runErr error) {
	args := dockerRunArgs(d.Image, command, d.resolvedCwd, stdin)

	cmd := dockerCommand(ctx, args)
	if stdin {
		cmd.Stdin = os.Stdin
	}

	outWriter := newCaptureWriter(maxBytes)
	errWriter := newCaptureWriter(maxBytes)

	if streamStdout {
		cmd.Stdout = io.MultiWriter(os.Stdout, outWriter)
	} else {
		cmd.Stdout = outWriter
	}

	if streamStderr {
		cmd.Stderr = io.MultiWriter(os.Stderr, errWriter)
	} else {
		cmd.Stderr = errWriter
	}

	runErr = cmd.Run()

	return outWriter.result(), errWriter.result(), runErr
}

// Run runs command in a container, streaming stdout/stderr live and wiring
// the host's stdin through (-i). Any nonzero exit is a Go error.
func (d DockerRunner) Run(ctx context.Context, command string) error {
	slog.Debug("shell.docker.run", "image", d.Image, "command", command, "cwd", d.resolvedCwd)

	_, _, runErr := d.dockerExec(ctx, command, true, true, true, 0)

	slog.Debug("shell.docker.run", "image", d.Image, "command", command, "cwd", d.resolvedCwd, "exit_code", exitCodeOf(runErr))

	if runErr != nil {
		return fmt.Errorf("command %q failed in image %q: %w", command, d.Image, wrapIfCanceled(ctx, runErr))
	}

	return nil
}

// RunCapture runs command in a container, capturing stdout and stderr while
// also streaming stderr live. The captured output is logged (at debug
// level) on both success and failure, matching HostRunner.RunCapture, so a
// failing containerized check/out command's output is available for
// debugging. Any nonzero exit is a Go error.
func (d DockerRunner) RunCapture(ctx context.Context, command string) ([]byte, error) {
	slog.Debug("shell.docker.capture", "image", d.Image, "command", command, "cwd", d.resolvedCwd)

	stdout, stderr, runErr := d.dockerExec(ctx, command, true, false, true, 0)

	slog.Debug("shell.docker.capture", "image", d.Image, "command", command, "cwd", d.resolvedCwd,
		"exit_code", exitCodeOf(runErr), "output_bytes", len(stdout), "output", stdout, "stderr", stderr)

	if runErr != nil {
		return nil, fmt.Errorf("command %q failed in image %q: %w", command, d.Image, wrapIfCanceled(ctx, runErr))
	}

	return []byte(stdout), nil
}

// RunCaptureFull runs command in a container, capturing stdout/stderr
// separately with no stdin attached. Docker-level failures (bad image,
// daemon unreachable) surface via docker run's own exit codes (commonly 125
// for daemon-side errors, 126/127 for a command the container couldn't
// run/find) exactly like any other nonzero exit — as data via exitCode, not
// a Go error, even a signal-killed one (e.g. from a canceled ctx). Only a
// failure to start the docker CLI client itself returns a non-nil error. A
// caller that needs to tell "this result may be incomplete because ctx was
// canceled while the command ran" apart from an ordinary exit code checks
// ctx.Err() itself (or CanceledError) after this returns, rather than
// relying on err.
func (d DockerRunner) RunCaptureFull(ctx context.Context, command string) (stdout, stderr string, exitCode int, err error) {
	return d.runCaptureFull(ctx, command, 0)
}

// RunCaptureFullLimited is RunCaptureFull with each stream capped at
// maxBytes while the command runs — see the Runner interface doc.
func (d DockerRunner) RunCaptureFullLimited(ctx context.Context, command string, maxBytes int) (stdout, stderr string, exitCode int, err error) {
	return d.runCaptureFull(ctx, command, maxBytes)
}

// runCaptureFull is the shared implementation behind RunCaptureFull (maxBytes
// 0, meaning unbounded — byte-identical to before RunCaptureFullLimited
// existed) and RunCaptureFullLimited (maxBytes > 0).
func (d DockerRunner) runCaptureFull(ctx context.Context, command string, maxBytes int) (stdout, stderr string, exitCode int, err error) {
	slog.Debug("shell.docker.capture_full", "image", d.Image, "command", command, "cwd", d.resolvedCwd)

	stdout, stderr, runErr := d.dockerExec(ctx, command, false, false, false, maxBytes)

	if !processStarted(runErr) {
		return "", "", -1, fmt.Errorf("docker run failed to start for image %q: %w", d.Image, runErr)
	}

	code := exitCodeOf(runErr)

	slog.Debug("shell.docker.capture_full", "image", d.Image, "command", command, "cwd", d.resolvedCwd, "exit_code", code)

	return stdout, stderr, code, nil
}

// dockerCommand builds the exec.Cmd for `docker <args...>`, wired so a
// context cancellation sends SIGTERM to the docker CLI client (sig-proxied
// into the container by default, since no -t is ever passed) rather than an
// immediate SIGKILL, giving the containerized command a chance to exit
// cleanly before the grace period expires.
func dockerCommand(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // executing pipeline-defined commands in a pipeline-defined image is this tool's purpose
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = dockerKillGrace

	return cmd
}

// dockerRunArgs builds the argv (after "docker") for one containerized
// command: `run --rm --init [-i] [-v resolvedCwd:resolvedCwd -w resolvedCwd]
// -- image sh -c command`. resolvedCwd is already an absolute, symlink-free
// path (see resolveMountPath, called once by NewRunner at construction —
// not here, so repeated calls against the same DockerRunner don't
// re-resolve it) mounted at that identical host path so host-side readers
// of the same directory (agent read_file/list_dir, workspace Capture) stay
// coherent. An empty resolvedCwd (only resource check: today) mounts
// nothing and runs in the image's default workdir. --init supplies a real
// PID 1 so SIGTERM (see dockerCommand) actually reaches the command. The
// literal "--" immediately before image is load-bearing, not decorative: it
// tells docker's flag parser that everything after it is positional, so an
// image value docker's parser would otherwise read as a flag (e.g.
// "--privileged", "-v /:/host") can't be smuggled into the docker run
// invocation itself — it can only ever be looked up as an (invalid) image
// name. config.validateImageValues rejects such a value at LoadConfig time
// too; this is defense in depth for any image string that reaches here by
// another path.
func dockerRunArgs(image, command, resolvedCwd string, stdin bool) []string {
	args := []string{"run", "--rm", "--init"}

	if stdin {
		args = append(args, "-i")
	}

	if resolvedCwd != "" {
		args = append(args, "-v", resolvedCwd+":"+resolvedCwd, "-w", resolvedCwd)
	}

	args = append(args, "--", image, "sh", "-c", command)

	return args
}

// resolveMountPath returns cwd as an absolute path with symlinks resolved,
// so the host path handed to `docker run -v` matches the real filesystem
// location Docker Desktop (or the daemon) actually shares. Rejects a
// resolved path containing ':' — docker's own `-v host:container` volume
// spec splits on that character, so a path containing one would be silently
// misparsed into the wrong mount (or rejected by docker with a confusing
// error) rather than failing clearly here.
func resolveMountPath(cwd string) (string, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	if strings.Contains(resolved, ":") {
		return "", fmt.Errorf("path %q contains ':', which is not supported for a docker bind mount", resolved)
	}

	return resolved, nil
}

// ValidateDocker fails fast when a pipeline configures image: but docker
// isn't usable: the docker CLI must be on PATH and `docker info` must
// succeed (daemon reachable). Mirrors internal/workspace's Provider.
// Validate() precedent — check once at startup, before any step runs.
func ValidateDocker(ctx context.Context) error {
	_, err := exec.LookPath("docker")
	if err != nil {
		return errors.New("docker CLI not found on PATH, but this pipeline configures image")
	}

	var errBuf bytes.Buffer

	cmd := exec.CommandContext(ctx, "docker", "info")
	cmd.Stderr = &errBuf

	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("docker daemon unreachable (docker info failed: %s): %w", errBuf.String(), err)
	}

	return nil
}
