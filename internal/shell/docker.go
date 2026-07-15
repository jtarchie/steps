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
// call. The working directory is bind-mounted at its own (resolved) host
// path and set as the container's workdir, so host-side readers (agent
// read_file/list_dir, workspace Capture) see the same files a containerized
// command wrote. No host environment variables are passed through — the
// container starts from the image's own env only.
type DockerRunner struct {
	Image string
}

// Run runs command in a container, streaming stdout/stderr live and wiring
// the host's stdin through (-i). Any nonzero exit is a Go error.
func (d DockerRunner) Run(ctx context.Context, command, cwd string) error {
	args, resolvedCwd, err := dockerRunArgs(d.Image, command, cwd, true)
	if err != nil {
		return err
	}

	slog.Debug("shell.docker.run", "image", d.Image, "command", command, "cwd", resolvedCwd)

	cmd := dockerCommand(ctx, args)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	runErr := cmd.Run()

	slog.Debug("shell.docker.run", "image", d.Image, "command", command, "cwd", resolvedCwd, "exit_code", exitCodeOf(runErr))

	if runErr != nil {
		return fmt.Errorf("command %q failed in image %q: %w", command, d.Image, runErr)
	}

	return nil
}

// RunCapture runs command in a container, capturing stdout and stderr while
// also streaming stderr live. The captured output is logged (at debug
// level) on both success and failure, matching HostRunner.RunCapture, so a
// failing containerized check/out command's output is available for
// debugging. Any nonzero exit is a Go error.
func (d DockerRunner) RunCapture(ctx context.Context, command, cwd string) ([]byte, error) {
	args, resolvedCwd, err := dockerRunArgs(d.Image, command, cwd, true)
	if err != nil {
		return nil, err
	}

	slog.Debug("shell.docker.capture", "image", d.Image, "command", command, "cwd", resolvedCwd)

	cmd := dockerCommand(ctx, args)
	cmd.Stdin = os.Stdin

	var outBuf, errBuf bytes.Buffer

	cmd.Stdout = &outBuf
	cmd.Stderr = io.MultiWriter(os.Stderr, &errBuf)

	runErr := cmd.Run()

	slog.Debug("shell.docker.capture", "image", d.Image, "command", command, "cwd", resolvedCwd,
		"exit_code", exitCodeOf(runErr), "output_bytes", outBuf.Len(), "output", outBuf.String(), "stderr", errBuf.String())

	if runErr != nil {
		return nil, fmt.Errorf("command %q failed in image %q: %w", command, d.Image, runErr)
	}

	return outBuf.Bytes(), nil
}

// RunCaptureFull runs command in a container, capturing stdout/stderr
// separately with no stdin attached. Docker-level failures (bad image,
// daemon unreachable) surface via docker run's own exit codes (commonly 125
// for daemon-side errors, 126/127 for a command the container couldn't
// run/find) exactly like any other nonzero exit — as data via exitCode, not
// a Go error. Only a failure to start the docker CLI client itself returns a
// non-nil error.
func (d DockerRunner) RunCaptureFull(ctx context.Context, command, cwd string) (stdout, stderr string, exitCode int, err error) {
	args, resolvedCwd, err := dockerRunArgs(d.Image, command, cwd, false)
	if err != nil {
		return "", "", -1, err
	}

	slog.Debug("shell.docker.capture_full", "image", d.Image, "command", command, "cwd", resolvedCwd)

	cmd := dockerCommand(ctx, args)
	cmd.Stdin = nil

	var outBuf, errBuf bytes.Buffer

	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()

	if !processStarted(runErr) {
		return "", "", -1, fmt.Errorf("docker run failed to start for image %q: %w", d.Image, runErr)
	}

	code := exitCodeOf(runErr)

	slog.Debug("shell.docker.capture_full", "image", d.Image, "command", command, "cwd", resolvedCwd, "exit_code", code)

	return outBuf.String(), errBuf.String(), code, nil
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
// command: `run --rm --init [-i] [-v cwd:cwd -w cwd] image sh -c command`.
// cwd is resolved to an absolute, symlink-free path before mounting (macOS
// temp dirs live under /var/folders, which Docker Desktop only shares via
// its /private-prefixed real path) and mounted at that identical path so
// host-side readers of the same directory (agent read_file/list_dir,
// workspace Capture) stay coherent. An empty cwd (only resource check: today)
// mounts nothing and runs in the image's default workdir. --init supplies a
// real PID 1 so SIGTERM (see dockerCommand) actually reaches the command.
func dockerRunArgs(image, command, cwd string, stdin bool) (args []string, resolvedCwd string, err error) {
	args = []string{"run", "--rm", "--init"}

	if stdin {
		args = append(args, "-i")
	}

	if cwd != "" {
		resolvedCwd, err = resolveMountPath(cwd)
		if err != nil {
			return nil, "", fmt.Errorf("resolve working directory %q: %w", cwd, err)
		}

		args = append(args, "-v", resolvedCwd+":"+resolvedCwd, "-w", resolvedCwd)
	}

	args = append(args, image, "sh", "-c", command)

	return args, resolvedCwd, nil
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
