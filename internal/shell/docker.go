package shell

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// dockerKillGrace bounds how long a canceled docker CLI client is given to
// exit cleanly (SIGTERM) before Go force-kills it. Unlike the per-command
// `docker run` this replaced, a force-killed client can no longer strand a
// container: the container is named and owned by the session, so Close's
// `docker rm -f` tears it down regardless of what happened to any individual
// exec client.
const dockerKillGrace = 10 * time.Second

// dockerCleanupTimeout bounds the `docker rm -f` a session issues at Close,
// which runs from cleanup paths that must not depend on a caller's (possibly
// already-canceled) context — mirroring internal/workspace's btrfs backend.
const dockerCleanupTimeout = 30 * time.Second

// dockerSessionLifetime is how long a step's container stays alive on its
// own. Nothing normally relies on it: Close removes the container as soon as
// the step is done. It exists so that a steps process killed outright
// (SIGKILL, a panicking host, a pulled plug) — which never gets to run Close
// — still leaves no container behind indefinitely, because the keepalive
// exits on its own and `--rm` reaps it. A step legitimately running longer
// than this would see its container vanish mid-run, so it is set far above
// any plausible step duration rather than tuned tightly.
const dockerSessionLifetime = 24 * time.Hour

// DockerRunner runs commands inside one container per step: the session is
// started lazily on the first command and torn down by Close.
//
// It used to be a fresh `docker run --rm` per command, which had three
// problems this shape fixes at once. State did not carry between calls — an
// agent's `pip install x` followed by `python y` as two run_shell calls
// simply did not work, and no amount of prompting reliably stops a model from
// trying. Every call paid full container-start latency, which an agent
// conversation pays dozens of times. And a force-killed client stranded a
// container the daemon only reaped later, because nothing on our side knew
// its name.
//
// The working directory is bind-mounted at its own (resolved) host path and
// set as the container's workdir, so host-side readers (agent
// read_file/list_dir, workspace Capture) see the same files a containerized
// command wrote. No host environment variables are passed through — the
// container starts from the image's own env only.
type DockerRunner struct {
	Image   string
	label   string
	session *dockerSession
}

// WithLabel returns a copy of d that prefixes its live-streamed output —
// see the Runner interface doc. The copy shares the same container session,
// so a labeled runner is the same step's container, not a second one.
func (d DockerRunner) WithLabel(label string) Runner {
	d.label = label

	return d
}

// Close tears down the step's container. Safe to call when no command ever
// ran (nothing was started, so there is nothing to remove) and safe to call
// twice.
func (d DockerRunner) Close() error {
	return d.session.close()
}

// dockerSession owns one step's container: its name, its lazy start, and its
// teardown. DockerRunner holds it by pointer so WithLabel's copy shares the
// same container.
type dockerSession struct {
	image       string
	resolvedCwd string
	// envNames are the variables the pipeline's env: opted this command into.
	// They are passed at container start, so every exec in the session
	// inherits them.
	envNames []string

	mu sync.Mutex
	// attempted records that start has been tried, so a failure is sticky:
	// a bad image must not be re-pulled once per command in an agent's
	// conversation.
	attempted bool
	name      string
	startOut  string
	startErr  error
}

// ensure starts the container if it has not been started yet, returning its
// name.
//
// A failure to start is reported exactly as the underlying `docker run`
// reported it — the same *exec.ExitError, carrying the same exit code (125
// for a daemon-side error such as an unknown image) and the same stderr that
// a per-command `docker run` used to produce. That is what keeps a bad image
// surfacing to an agent as ordinary tool-result data rather than a crash, and
// keeps a containerized task's failure classifying as a task failure via
// IsExitError, exactly as before this became a session.
func (s *dockerSession) ensure(ctx context.Context) (name, stderr string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.attempted {
		return s.name, s.startOut, s.startErr
	}

	s.attempted = true

	containerName, err := newContainerName()
	if err != nil {
		s.startErr = err

		return "", "", err
	}

	args := dockerStartArgs(s.image, containerName, s.resolvedCwd, s.envNames)

	slog.Debug("shell.docker.session_start", "image", s.image, "container", containerName, "cwd", s.resolvedCwd)

	cmd := dockerCommand(ctx, args)

	var errBuf bytes.Buffer

	cmd.Stdout = io.Discard // the container id; we already know its name
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	if runErr != nil {
		s.startOut = errBuf.String()
		// Deliberately NOT wrapped: callers inspect this with errors.As for
		// *exec.ExitError (IsExitError, exitCodeOf, processStarted) to decide
		// whether a containerized command failed or never ran, and wrapping
		// text around it here would be cosmetic while the shape is load-bearing.
		s.startErr = runErr

		slog.Debug("shell.docker.session_start_failed", "image", s.image, "exit_code", exitCodeOf(runErr), "stderr", s.startOut)

		return "", s.startOut, s.startErr
	}

	s.name = containerName

	return s.name, "", nil
}

// close removes the container. It builds its own bounded context rather than
// taking the caller's: Close runs from deferred cleanup paths whose context is
// routinely already canceled (a timed-out step, Ctrl-C), and those are exactly
// the cases where leaving a container running would be worst.
func (s *dockerSession) close() error {
	s.mu.Lock()
	name := s.name
	s.name = ""
	s.mu.Unlock()

	if name == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
	defer cancel()

	slog.Debug("shell.docker.session_close", "container", name)

	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", name).CombinedOutput() //nolint:gosec // name is a hex string this package generated
	if err != nil {
		return fmt.Errorf("removing container %s: %w: %s", name, err, out)
	}

	return nil
}

// newContainerName mints a random, collision-free container name. Random
// rather than derived from the step's name: two runs of the same step (a
// watch loop, a retried attempt) must never contend for one name, and a name
// we generated is one Close can remove knowing nothing else could own it.
func newContainerName() (string, error) {
	var buf [8]byte

	_, err := rand.Read(buf[:])
	if err != nil {
		return "", fmt.Errorf("generating a container name: %w", err)
	}

	return fmt.Sprintf("steps-%x", buf), nil
}

// dockerExec is the shared plumbing behind Run/RunCapture/RunCaptureFull/
// RunCaptureFullLimited/RunCaptureFullLimitedStreamed: it always captures
// both stdout and stderr — even for Run, which never exposes them, trading a
// little memory for one less code path to keep in sync across five
// near-identical methods — additionally streaming either live (to
// os.Stdout/os.Stderr, prefixed when WithLabel was used) when
// streamStdout/streamStderr is set. stdin wires the host's stdin through
// only when set; otherwise cmd.Stdin stays nil (/dev/null), matching
// RunCaptureFull's non-interactive semantics for model-generated commands.
// maxBytes caps each captured stream (0 means unbounded — every caller but
// RunCaptureFullLimited/RunCaptureFullLimitedStreamed passes 0, reproducing
// today's behavior exactly). spillDir, when set alongside a positive
// maxBytes, streams overflow to a file under that (host) directory instead
// of dropping it — see newCaptureWriter/spillWriter. The writer runs
// host-side regardless of where command executes, so the resulting spill
// file is always a host path.
func (d DockerRunner) dockerExec(
	ctx context.Context, command string, stdin, streamStdout, streamStderr bool, maxBytes int, spillDir string,
) (stdout, stderr string, runErr error) {
	name, startStderr, startErr := d.session.ensure(ctx)
	if startErr != nil {
		return "", startStderr, startErr
	}

	cmd := dockerCommand(ctx, dockerExecArgs(name, command, stdin))
	if stdin {
		cmd.Stdin = os.Stdin
	}

	outWriter := newCaptureWriter(maxBytes, spillDir)
	errWriter := newCaptureWriter(maxBytes, spillDir)

	flushStdout, flushStderr := func() {}, func() {}

	if streamStdout {
		var stdoutW io.Writer

		stdoutW, flushStdout = prefixedStream(d.label, os.Stdout)
		cmd.Stdout = io.MultiWriter(stdoutW, outWriter)
	} else {
		cmd.Stdout = outWriter
	}

	if streamStderr {
		var stderrW io.Writer

		stderrW, flushStderr = prefixedStream(d.label, os.Stderr)
		cmd.Stderr = io.MultiWriter(stderrW, errWriter)
	} else {
		cmd.Stderr = errWriter
	}

	runErr = cmd.Run()
	flushStdout()
	flushStderr()

	return outWriter.result(), errWriter.result(), runErr
}

// Run runs command in the step's container, streaming stdout/stderr live and
// wiring the host's stdin through (-i). Any nonzero exit is a Go error.
func (d DockerRunner) Run(ctx context.Context, command string) error {
	slog.Debug("shell.docker.run", "image", d.Image, "command", command, "cwd", d.session.resolvedCwd)

	_, _, runErr := d.dockerExec(ctx, command, true, true, true, 0, "")

	slog.Debug("shell.docker.run", "image", d.Image, "command", command, "exit_code", exitCodeOf(runErr))

	if runErr != nil {
		return fmt.Errorf("command %q failed in image %q: %w", command, d.Image, wrapIfCanceled(ctx, runErr))
	}

	return nil
}

// RunCapture runs command in the step's container, capturing stdout and
// stderr while also streaming stderr live. The captured output is logged (at
// debug level) on both success and failure, matching HostRunner.RunCapture, so
// a failing containerized check/out command's output is available for
// debugging. Any nonzero exit is a Go error.
func (d DockerRunner) RunCapture(ctx context.Context, command string) ([]byte, error) {
	slog.Debug("shell.docker.capture", "image", d.Image, "command", command, "cwd", d.session.resolvedCwd)

	stdout, stderr, runErr := d.dockerExec(ctx, command, true, false, true, 0, "")

	slog.Debug("shell.docker.capture", "image", d.Image, "command", command,
		"exit_code", exitCodeOf(runErr), "output_bytes", len(stdout), "output", stdout, "stderr", stderr)

	if runErr != nil {
		return nil, fmt.Errorf("command %q failed in image %q: %w", command, d.Image, wrapIfCanceled(ctx, runErr))
	}

	return []byte(stdout), nil
}

// RunCaptureFull runs command in the step's container, capturing
// stdout/stderr separately with no stdin attached. Docker-level failures (bad
// image, daemon unreachable) surface via docker's own exit codes (commonly 125
// for daemon-side errors, 126/127 for a command the container couldn't
// run/find) exactly like any other nonzero exit — as data via exitCode, not
// a Go error, even a signal-killed one (e.g. from a canceled ctx). Only a
// failure to start the docker CLI client itself returns a non-nil error. A
// caller that needs to tell "this result may be incomplete because ctx was
// canceled while the command ran" apart from an ordinary exit code checks
// ctx.Err() itself (or CanceledError) after this returns, rather than
// relying on err.
func (d DockerRunner) RunCaptureFull(ctx context.Context, command string) (stdout, stderr string, exitCode int, err error) {
	return d.runCaptureFull(ctx, command, 0, "", false)
}

// RunCaptureFullLimited is RunCaptureFull with each stream capped at
// maxBytes (and, with spillDir set, overflow streamed to disk instead of
// dropped) while the command runs — see the Runner interface doc.
func (d DockerRunner) RunCaptureFullLimited(ctx context.Context, command string, maxBytes int, spillDir string) (stdout, stderr string, exitCode int, err error) {
	return d.runCaptureFull(ctx, command, maxBytes, spillDir, false)
}

// RunCaptureFullLimitedStreamed is RunCaptureFullLimited, additionally
// streaming both stdout/stderr live (prefixed, when WithLabel was used) —
// see the Runner interface doc.
func (d DockerRunner) RunCaptureFullLimitedStreamed(ctx context.Context, command string, maxBytes int, spillDir string) (stdout, stderr string, exitCode int, err error) {
	return d.runCaptureFull(ctx, command, maxBytes, spillDir, true)
}

// runCaptureFull is the shared implementation behind RunCaptureFull (maxBytes
// 0, meaning unbounded — byte-identical to before RunCaptureFullLimited
// existed), RunCaptureFullLimited (maxBytes > 0), and
// RunCaptureFullLimitedStreamed (stream true).
func (d DockerRunner) runCaptureFull(ctx context.Context, command string, maxBytes int, spillDir string, stream bool) (stdout, stderr string, exitCode int, err error) {
	slog.Debug("shell.docker.capture_full", "image", d.Image, "command", command, "cwd", d.session.resolvedCwd)

	stdout, stderr, runErr := d.dockerExec(ctx, command, false, stream, stream, maxBytes, spillDir)

	if !processStarted(runErr) {
		return "", "", -1, fmt.Errorf("docker failed to start for image %q: %w", d.Image, runErr)
	}

	code := exitCodeOf(runErr)

	slog.Debug("shell.docker.capture_full", "image", d.Image, "command", command, "exit_code", code)

	return stdout, stderr, code, nil
}

// dockerCommand builds the exec.Cmd for `docker <args...>`, wired so a
// context cancellation sends SIGTERM to the docker CLI client rather than an
// immediate SIGKILL, giving it a chance to exit cleanly before the grace
// period expires. Whatever the client does, the command running inside the
// container is bounded by the session's own teardown.
func dockerCommand(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // executing pipeline-defined commands in a pipeline-defined image is this tool's purpose
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = dockerKillGrace

	return cmd
}

// dockerStartArgs builds the argv (after "docker") that starts a step's
// container: `run -d --rm --init --name <name> [-v resolvedCwd:resolvedCwd -w
// resolvedCwd] -- image sh -c sleep <lifetime>`.
//
// resolvedCwd is already an absolute, symlink-free path (see
// resolveMountPath, called once by NewRunner at construction) mounted at that
// identical host path so host-side readers of the same directory (agent
// read_file/list_dir, workspace Capture) stay coherent. An empty resolvedCwd
// (only resource check: today) mounts nothing and runs in the image's default
// workdir.
//
// --init supplies a real PID 1. That matters more here than it did for a
// one-shot `docker run`: processes an exec leaves behind reparent to PID 1,
// and the keepalive is a `sleep` that would never reap them, so without tini
// a long agent conversation would accumulate zombies in its own container.
//
// The keepalive is a bounded `sleep` rather than an endless loop, paired with
// --rm: see dockerSessionLifetime for why an abandoned container has to be
// able to exit on its own. It needs nothing from the image beyond the `sh`
// and `sleep` that running any command through `sh -c` already assumes.
//
// The literal "--" immediately before image is load-bearing, not decorative:
// it tells docker's flag parser that everything after it is positional, so an
// image value docker's parser would otherwise read as a flag (e.g.
// "--privileged", "-v /:/host") can't be smuggled into the docker run
// invocation itself — it can only ever be looked up as an (invalid) image
// name. config.validateImageValues rejects such a value at LoadConfig time
// too; this is defense in depth for any image string that reaches here by
// another path.
func dockerStartArgs(image, name, resolvedCwd string, envNames []string) []string {
	args := []string{"run", "-d", "--rm", "--init", "--name", name}

	if resolvedCwd != "" {
		args = append(args, "-v", resolvedCwd+":"+resolvedCwd, "-w", resolvedCwd)
	}

	// `-e NAME` (no value) tells the docker CLI to forward the value from its
	// OWN environment, which is ours. Spelling it `-e NAME=value` instead
	// would put the secret in the docker client's argv, where anything able
	// to read the host's process list could see it for as long as the command
	// runs — a worse exposure than the one env: exists to avoid. A name that
	// is unset here is simply not set in the container.
	for _, name := range envNames {
		args = append(args, "-e", name)
	}

	keepalive := fmt.Sprintf("sleep %d", int(dockerSessionLifetime.Seconds()))

	return append(args, "--", image, "sh", "-c", keepalive)
}

// dockerExecArgs builds the argv (after "docker") for one command in an
// already-running session container: `exec [-i] -- <name> sh -c command`. The
// container's workdir was set at start, so exec inherits it rather than
// repeating -w.
func dockerExecArgs(name, command string, stdin bool) []string {
	args := []string{"exec"}

	if stdin {
		args = append(args, "-i")
	}

	return append(args, "--", name, "sh", "-c", command)
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
