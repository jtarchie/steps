package shell

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/dockerapi"
)

// errSessionClosed is returned when a command is issued through a runner
// whose container has already been torn down.
var errSessionClosed = errors.New("shell: the step's container has been closed")

// dockerCleanupTimeout bounds the removal a session issues at Close, which
// runs from cleanup paths that must not depend on a caller's (possibly
// already-canceled) context — mirroring internal/workspace's btrfs backend.
const dockerCleanupTimeout = 30 * time.Second

// dockerSessionLifetime is how long a step's container stays alive on its
// own. Nothing normally relies on it: Close removes the container as soon as
// the step is done. It exists so that a steps process killed outright
// (SIGKILL, a panicking host, a pulled plug) — which never gets to run Close
// — leaves a container that at least stops CONSUMING anything: the keepalive
// exits and the container becomes an inert Exited row, which
// SweepOrphanedContainers removes on the next run. A step legitimately
// running longer than this would see its container die mid-run, so it is set
// far above any plausible step duration rather than tuned tightly.
const dockerSessionLifetime = 24 * time.Hour

// DockerRunner runs commands inside one container per step: the session is
// started lazily on the first command and torn down by Close.
//
// It used to be a fresh throwaway container per command, which had three
// problems this shape fixes at once. State did not carry between calls — an
// agent's `pip install x` followed by `python y` as two run_shell calls
// simply did not work, and no amount of prompting reliably stops a model from
// trying. Every call paid full container-start latency, which an agent
// conversation pays dozens of times. And a container the caller lost track of
// was reaped only much later, because nothing on our side knew its name.
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
	// dockerHost is the daemon this session's containers live on. Empty is
	// this machine's.
	dockerHost string
	// envNames are the variables the pipeline's env: opted this command into.
	// They are passed at container start, so every exec in the session
	// inherits them.
	envNames []string
	// envValues are variables the caller supplies with their values, for the
	// ones this process's own environment does not hold — a venue's
	// STEPS_WORKER.
	envValues map[string]string
	// user is the already-resolved --user value (see containerUser); empty
	// takes the image's own default.
	user string
	// network is the --network value; empty takes docker's own default.
	network string
	// privileged adds --privileged.
	privileged bool
	// cpuShares/memoryBytes are --cpu-shares and --memory; zero omits each.
	cpuShares   int
	memoryBytes int64

	mu sync.Mutex
	// client is the connection to the daemon, opened with the container and
	// released by close. Held rather than reopened per command: connecting is
	// the expensive half of an exec against a container that is already up.
	client *dockerapi.Client
	// attempted records that start has been tried, so a failure is sticky:
	// a bad image must not be re-pulled once per command in an agent's
	// conversation.
	attempted bool
	// closed records that the session was torn down, so a command issued
	// afterwards fails saying so. Without it, close's clearing of name would
	// leave attempted set and startErr nil, and the next command would exec
	// against an empty container name — a malformed docker invocation
	// reported as an opaque exit code.
	closed bool
	// name is what the container was called, and id is what the daemon calls
	// it. Both are kept: the name is what a sweep and a human recognise, the
	// id is what every later request names.
	name     string
	id       string
	startOut string
	startErr error
}

// ensure starts the container if it has not been started yet, returning its
// id.
//
// A daemon-side refusal — an unknown image is the one that matters — is
// reported as an *ExitError carrying 125, the status `docker run` exits with
// for exactly this. That is what keeps a bad image surfacing to an agent
// as ordinary tool-result data rather than a crash, and keeps a containerized
// task's failure classifying as a task failure via IsExitError. A container
// that started and then DIED is a different answer and deliberately not an
// ExitError: see checkAlive.
func (s *dockerSession) ensure(ctx context.Context) (id, stderr string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return "", "", errSessionClosed
	}

	if s.attempted {
		return s.id, s.startOut, s.startErr
	}

	s.attempted = true

	containerName, err := NewContainerName()
	if err != nil {
		s.startErr = err

		return "", "", err
	}

	client, err := dockerapi.New(s.dockerHost)
	if err != nil {
		s.startErr = fmt.Errorf("connecting to the daemon for image %q: %w", s.image, err)

		return "", "", s.startErr
	}

	s.client = client

	slog.Debug("shell.docker.session_start", "image", s.image, "container", containerName, "cwd", s.resolvedCwd)

	containerID, startErr := s.start(ctx, containerName)
	if startErr != nil {
		s.startOut = startErr.Error()
		s.startErr = &ExitError{Command: "starting a container from image " + s.image, Code: dockerDaemonRefusedCode}

		slog.Debug("shell.docker.session_start_failed", "image", s.image, "error", startErr)

		return "", s.startOut, s.startErr
	}

	// Starting reports that the container STARTED, not that it is still up:
	// it succeeds for one that died a millisecond later. That is the normal
	// outcome for an image with an ENTRYPOINT, since the keepalive becomes
	// arguments TO that entrypoint rather than replacing it — alpine/git runs
	// `git sh -c ...` and exits. Taking success at face value left every later
	// exec reporting a container that does not exist, which says nothing about
	// the actual cause.
	deadErr := s.checkAlive(ctx, containerID)
	if deadErr != nil {
		s.startOut = deadErr.Error()
		s.startErr = deadErr

		slog.Debug("shell.docker.session_died", "image", s.image, "container", containerName, "error", deadErr)

		// The corpse has told us what we needed; take it away now rather than
		// leaving it for the next run's sweep. s.id is deliberately left
		// unset, so close has nothing to do for this session.
		reclaim(removalContext(ctx), s.client, containerID)

		return "", s.startOut, s.startErr
	}

	s.name, s.id = containerName, containerID

	return s.id, "", nil
}

// dockerDaemonRefusedCode is the status a daemon-side refusal reports. 125 is
// what `docker run` exits with for one, and the value is load-bearing rather
// than cosmetic: docs/infra.md documents it, and an agent shown this as a tool
// result sees the same number it always did.
const dockerDaemonRefusedCode = 125

// start creates and starts the session's container.
func (s *dockerSession) start(ctx context.Context, name string) (string, error) {
	spec := dockerapi.ContainerSpec{
		Image:       s.image,
		Cmd:         []string{"sh", "-c", keepAliveCommand()},
		Name:        name,
		WorkingDir:  s.resolvedCwd,
		Env:         s.containerEnv(),
		Labels:      OwnershipLabels(),
		User:        s.user,
		Network:     s.network,
		Privileged:  s.privileged,
		CPUShares:   int64(s.cpuShares),
		MemoryBytes: s.memoryBytes,
		// A real PID 1, so processes an exec leaves behind are reaped rather
		// than accumulating in a long agent conversation's container.
		Init: true,
		// Deliberately NOT self-removing: a container that removes itself
		// takes its own postmortem with it, and the postmortem is the whole
		// diagnosis when an image rejects the keepalive.
		AutoRemove: false,
	}

	id, err := s.client.CreateContainer(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	err = s.client.StartContainer(ctx, id)
	if err != nil {
		// Created but not running: nothing else will ever name it, so it has
		// to be reclaimed here.
		reclaim(removalContext(ctx), s.client, id)

		return "", fmt.Errorf("%w", err)
	}

	return id, nil
}

// checkAlive reports why the container is no longer running, or nil if it is.
//
// The diagnosis is the point: the container's own exit code and first lines of
// output are what say the image rejected the command, and they are gone the
// moment anything removes it. This is why the container is NOT created with
// AutoRemove — a self-removing container takes its own postmortem with it.
//
// Deliberately a plain error rather than an *ExitError. The container did not
// run the step's command and exit nonzero; it never got as far as accepting
// one, which is an infrastructure failure and has to classify as such.
func (s *dockerSession) checkAlive(ctx context.Context, id string) error {
	died, exitCode, err := s.client.SettleFor(ctx, id, dockerSettleBound)
	if err != nil {
		// Cannot tell; assume it is fine rather than failing a working step on
		// a question the daemon did not answer.
		slog.Debug("shell.docker.settle_failed", "container", id, "error", err)

		return nil
	}

	if !died {
		return nil
	}

	return fmt.Errorf("container for image %q exited immediately with code %d (its command was %q; an image with an ENTRYPOINT receives that as arguments rather than replacing it): %s",
		s.image, exitCode, keepAliveCommand(), logTailOrNothing(s.client.ContainerLogTail(ctx, id, dockerLogTailLines)))
}

// dockerSettleBound is how long a container is given to die at birth before
// it is taken to be up.
//
// Short, because every healthy containerized step pays it once. Long enough
// that an image whose entrypoint swallows the keepalive — which exits in
// single-digit milliseconds — is reliably caught, since the alternative is
// every later command reporting a container that does not exist and naming
// neither the image nor the reason.
const dockerSettleBound = 300 * time.Millisecond

// dockerLogTailLines is how much of a dead container's output is quoted back.
// Enough for an image's own error message, short enough to read in a step's
// failure.
const dockerLogTailLines = 10

// logTailOrNothing keeps a silent container from producing an error that
// trails off mid-sentence.
func logTailOrNothing(logs string) string {
	if logs == "" {
		return "(no output)"
	}

	return logs
}

// removalContext bounds a teardown that must not depend on the caller's own
// context.
//
// The likeliest reason a removal runs is that the caller was just cancelled,
// and a cleanup using that context would abort before reaching the daemon and
// leak the container it was reclaiming.
func removalContext(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// RemoveContainer deletes a container by name, best-effort. Exported so
// internal/agent can reclaim the one-shot container behind its containerized
// CLI run: that run has no session to Close, and nothing this end does stops
// a container, so its caller owns teardown.
func RemoveContainer(ctx context.Context, name string) {
	removeContainerOn(ctx, "", name)
}

// removeContainerOn is RemoveContainer against a named daemon.
func removeContainerOn(ctx context.Context, host, name string) {
	client, err := dockerapi.New(host)
	if err != nil {
		slog.Warn("shell.docker.remove_failed", "container", name, "error", err)

		return
	}

	defer func() { _ = client.Close() }()

	reclaim(ctx, client, name)
}

// reclaim removes a container, best effort.
//
// Best effort because every caller is on a teardown path: the container is
// already unwanted, and a failure to take it away is something to report
// rather than something to propagate over whatever was actually going wrong.
// The sweep on the next run is the backstop.
func reclaim(ctx context.Context, client *dockerapi.Client, id string) {
	err := client.RemoveContainer(ctx, id)
	if err != nil {
		slog.Warn("shell.docker.remove_failed", "container", id, "error", err)
	}
}

// close removes the container and releases the connection.
//
// It builds its own bounded context rather than taking the caller's: Close
// runs from deferred cleanup paths whose context is routinely already
// canceled (a timed-out step, Ctrl-C), and those are exactly the cases where
// leaving a container running would be worst.
func (s *dockerSession) close() error {
	s.mu.Lock()
	id, client := s.id, s.client
	s.id, s.name, s.client = "", "", nil
	s.closed = true
	s.mu.Unlock()

	if client == nil {
		return nil
	}

	defer func() { _ = client.Close() }()

	if id == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), dockerCleanupTimeout)
	defer cancel()

	slog.Debug("shell.docker.session_close", "container", id)

	// A container that is already gone is the outcome this wanted, not a
	// failure — it happens whenever the container exited on its own, the
	// keepalive expiring or a daemon restart — and RemoveContainer already
	// says so.
	reclaim(ctx, client, id)

	return nil
}

// NewContainerName mints a random, collision-free container name. Random
// rather than derived from the step's name: two runs of the same step (a
// watch loop, a retried attempt) must never contend for one name, and a name
// we generated is one Close can remove knowing nothing else could own it.
//
// Exported alongside RemoveContainer for internal/agent's one-shot CLI run,
// which needs the same "name it so you can always reclaim it" property
// without a session to hold the name for it.
func NewContainerName() (string, error) {
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
) (stdout, stderr string, exitCode int, runErr error) {
	id, startStderr, startErr := d.session.ensure(ctx)
	if startErr != nil {
		return "", startStderr, exitCodeOf(startErr), startErr
	}

	outWriter := newCaptureWriter(maxBytes, spillDir)
	errWriter := newCaptureWriter(maxBytes, spillDir)

	outTarget, flushStdout := io.Writer(outWriter), func() {}
	errTarget, flushStderr := io.Writer(errWriter), func() {}

	if streamStdout {
		var live io.Writer

		live, flushStdout = prefixedStream(d.label, os.Stdout)
		outTarget = io.MultiWriter(live, outWriter)
	}

	if streamStderr {
		var live io.Writer

		live, flushStderr = prefixedStream(d.label, os.Stderr)
		errTarget = io.MultiWriter(live, errWriter)
	}

	opts := dockerapi.ExecOptions{
		// The command is a shell string, so it runs through `sh -c` exactly as
		// it did — a pipeline writes shell, not an argv.
		Cmd:    []string{"sh", "-c", command},
		Stdout: outTarget,
		Stderr: errTarget,
	}

	if stdin {
		opts.Stdin = os.Stdin
	}

	code, execErr := d.session.client.Exec(ctx, id, opts)

	flushStdout()
	flushStderr()

	return outWriter.result(), errWriter.result(), code, execErr
}

// Run runs command in the step's container, streaming stdout/stderr live and
// wiring the host's stdin through (-i). Any nonzero exit is a Go error.
func (d DockerRunner) Run(ctx context.Context, command string) error {
	_, _, err := d.runStreamed(ctx, command, 0)

	return err
}

// RunStreamedCapture is Run, keeping what it streamed. See shell.Runner.
func (d DockerRunner) RunStreamedCapture(ctx context.Context, command string, maxBytes int) (string, string, error) {
	return d.runStreamed(ctx, command, maxBytes)
}

// runStreamed is the shared body of Run and RunStreamedCapture. dockerExec
// already returns what it streamed, so Run was discarding it; the only thing
// maxBytes changes is whether that buffering is bounded.
func (d DockerRunner) runStreamed(ctx context.Context, command string, maxBytes int) (stdout, stderr string, err error) {
	slog.Debug("shell.docker.run", "image", d.Image, "command", command, "cwd", d.session.resolvedCwd)

	stdout, stderr, code, runErr := d.dockerExec(ctx, command, true, true, true, maxBytes, "")

	slog.Debug("shell.docker.run", "image", d.Image, "command", command, "exit_code", code)

	failure := commandFailure(command, code, runErr)
	if failure != nil {
		return stdout, stderr, fmt.Errorf("command %q failed in image %q: %w", command, d.Image, wrapIfCanceled(ctx, failure))
	}

	return stdout, stderr, nil
}

// commandFailure turns a command's outcome into the error Run and RunCapture
// report, or nil for a clean one.
//
// The two halves classify differently and it matters. A command that ran and
// exited nonzero is an *ExitError, so the pipeline reads it as the step
// saying no. Anything that stopped the command from running at all — a
// container that would not stay up, a session already closed, a daemon that
// stopped answering — is passed through as it came, so it stays infrastructure
// and fires on_error rather than on_failure. A start the DAEMON refused is
// already an *ExitError by the time it gets here; see ensure.
func commandFailure(command string, code int, runErr error) error {
	if runErr != nil {
		return runErr
	}

	if code == 0 {
		return nil
	}

	return &ExitError{Command: command, Code: code}
}

// RunCapture runs command in the step's container, capturing stdout and
// stderr while also streaming stderr live. The captured output is logged (at
// debug level) on both success and failure, matching HostRunner.RunCapture, so
// a failing containerized check/out command's output is available for
// debugging. Any nonzero exit is a Go error.
func (d DockerRunner) RunCapture(ctx context.Context, command string) ([]byte, error) {
	slog.Debug("shell.docker.capture", "image", d.Image, "command", command, "cwd", d.session.resolvedCwd)

	stdout, stderr, code, runErr := d.dockerExec(ctx, command, true, false, true, 0, "")

	slog.Debug("shell.docker.capture", "image", d.Image, "command", command,
		"exit_code", code, "output_bytes", len(stdout), "output", stdout, "stderr", stderr)

	failure := commandFailure(command, code, runErr)
	if failure != nil {
		return nil, fmt.Errorf("command %q failed in image %q: %w", command, d.Image, wrapIfCanceled(ctx, failure))
	}

	return []byte(stdout), nil
}

// RunCaptureFull runs command in the step's container, capturing
// stdout/stderr separately with no stdin attached. Daemon-level refusals (a
// bad image) surface as the same 125 `docker run` reports, and a command the
// container could not run or find as its own 126/127 — exactly like any other
// nonzero exit, as data via exitCode rather than a Go error, even a
// signal-killed one (e.g. from a canceled ctx). Only a failure to have a
// container at all returns a non-nil error. A
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

	stdout, stderr, code, runErr := d.dockerExec(ctx, command, false, stream, stream, maxBytes, spillDir)

	// processStarted is what separates "the container refused this" — which is
	// an answer, reported as data — from "there was never a container to
	// refuse it", which is not.
	if runErr != nil && !processStarted(runErr) {
		return "", "", -1, fmt.Errorf("docker failed to start for image %q: %w", d.Image, runErr)
	}

	slog.Debug("shell.docker.capture_full", "image", d.Image, "command", command, "exit_code", code)

	return stdout, stderr, code, nil
}

// containerEnv resolves the pipeline's env: names into the NAME=value pairs
// the container is created with.
//
// The values travel in the request body rather than in any argument vector,
// which is strictly better than the `-e NAME` trick this replaced: that
// existed to keep a secret out of the docker client's argv, where anything
// able to read the host's process list could see it. There is no argv now.
//
// A name this process does not have is OMITTED rather than set empty, which
// is what lets a pipeline name an optional variable without every operator
// having to define it — set-but-empty is a different answer to a script that
// tests for the variable's presence.
//
// Caller-supplied values WIN over this process's own. A venue's STEPS_WORKER
// is the case: the pipeline's env: is the authority on the orchestrator, and
// the variable names a fact about the worker that nothing here has set.
func (s *dockerSession) containerEnv() []string {
	names := slices.Clone(s.envNames)

	for name := range s.envValues {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}

	slices.Sort(names)

	env := make([]string, 0, len(names))

	for _, name := range names {
		if value, ok := s.envValues[name]; ok {
			env = append(env, name+"="+value)

			continue
		}

		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}

	return env
}

// keepAliveCommand is what the session container runs so it stays up between
// execs: a bounded sleep, so a container nothing ever removes still stops on
// its own. It needs nothing from the image beyond the `sh` and `sleep` that
// running any command through `sh -c` already assumes.
func keepAliveCommand() string {
	return fmt.Sprintf("sleep %d", int(dockerSessionLifetime.Seconds()))
}

// ResolveMountPath returns cwd as an absolute path with symlinks resolved,
// so the host path handed to the daemon as a bind mount matches the real
// filesystem location Docker Desktop (or the daemon) actually shares. Rejects
// a resolved path containing ':' — a bind mount is spelled `host:container`,
// so a path containing one would be silently misparsed into the wrong mount
// (or rejected with a confusing error) rather than failing clearly here. Exported for internal/agent's
// containerized CLI run (DockerRunArgv), which must mount the same directory
// at the same resolved path the step's session container uses.
func ResolveMountPath(cwd string) (string, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	return resolved, checkMountPath(resolved)
}

// errMountPathColon is a bind mount source docker cannot be told about.
var errMountPathColon = errors.New("contains ':', which is not supported for a docker bind mount")

// checkMountPath refuses a path a bind mount cannot express.
//
// Separate from ResolveMountPath because a REMOTE mount path skips the
// resolution — only the worker can follow its own symlinks — but not this:
// a bind mount is `source:target`, so a colon anywhere turns one mount into a
// misparsed two, which the daemon either refuses confusingly or honours as
// something nobody asked for.
func checkMountPath(path string) error {
	if strings.Contains(path, ":") {
		return fmt.Errorf("path %q %w", path, errMountPathColon)
	}

	return nil
}

// ValidateDocker fails fast when a pipeline configures image: but the daemon
// is not usable. Mirrors internal/workspace's Provider.Validate() precedent —
// check once at startup, before any step runs.
//
// The daemon and nothing else. There used to be a docker BINARY half as well,
// because container execution spawned one; it does not, so a machine with a
// reachable daemon and no docker installed is now a perfectly good machine to
// run a containerized pipeline on.
//
// Which daemon is dockerapi's question, not this one's, and it is not the same
// question as "is DOCKER_HOST set" — see internal/dockerapi/host.go.
func ValidateDocker(ctx context.Context) error {
	client, err := dockerapi.New("")
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	defer func() { _ = client.Close() }()

	err = client.Ping(ctx)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}
