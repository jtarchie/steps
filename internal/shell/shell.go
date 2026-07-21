// Package shell runs pipeline-defined commands via `sh -c`, either on the
// host (HostRunner) or inside a container (DockerRunner, see docker.go).
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
)

// Runner runs pipeline-defined commands, either on the host or inside a
// container, against the working directory it was constructed with (see
// NewRunner) — binding cwd at construction, rather than taking it on every
// call, means a runner reused across many calls (an agent conversation's
// repeated run_shell calls, a task's fix-loop re-runs) resolves/validates
// its working directory once, not once per call.
type Runner interface {
	// Run streams stdout/stderr live; any nonzero exit is a Go error.
	Run(ctx context.Context, command string) error
	// RunCapture captures stdout while also streaming stderr live; any
	// nonzero exit is a Go error.
	RunCapture(ctx context.Context, command string) ([]byte, error)
	// RunCaptureFull captures stdout/stderr separately and never streams;
	// a normal nonzero exit is reported as data (exitCode), not an error —
	// only a failure to start the command returns a non-nil error.
	RunCaptureFull(ctx context.Context, command string) (stdout, stderr string, exitCode int, err error)
	// RunCaptureFullLimited behaves exactly like RunCaptureFull, except each
	// of stdout/stderr is capped at maxBytes while the command is running —
	// not just truncated afterward — so a runaway command's output never
	// fully buffers into memory before being discarded. A capped stream gets
	// a trailing "... [truncated N bytes]" marker, matching
	// internal/agent's truncateToolOutput format.
	RunCaptureFullLimited(ctx context.Context, command string, maxBytes int) (stdout, stderr string, exitCode int, err error)
}

// NewRunner returns a DockerRunner scoped to image and cwd, or a HostRunner
// when image is empty — the single decision point every shell-out caller
// (task, agent, resource) funnels through. For a DockerRunner, cwd is
// resolved and validated once here (see resolveMountPath); an empty cwd is
// valid (resource check: has none) and mounts nothing. HostRunner never
// errors — cwd needs no resolution for host execution.
func NewRunner(image, cwd string) (Runner, error) {
	if image == "" {
		return HostRunner{cwd: cwd}, nil
	}

	var resolvedCwd string

	if cwd != "" {
		var err error

		resolvedCwd, err = resolveMountPath(cwd)
		if err != nil {
			return nil, fmt.Errorf("resolve working directory %q: %w", cwd, err)
		}
	}

	return DockerRunner{Image: image, resolvedCwd: resolvedCwd}, nil
}

// captureWriter accumulates one stdout/stderr stream from a running command
// into a final string — used as cmd.Stdout/cmd.Stderr directly, so bytes are
// bounded (or not) as they arrive, never after the fact.
type captureWriter interface {
	io.Writer
	result() string
}

// newCaptureWriter returns an unboundedWriter when maxBytes <= 0 (matching
// every Run/RunCapture/RunCaptureFull caller's historical unbounded-buffer
// behavior exactly), or a boundedWriter otherwise (RunCaptureFullLimited).
func newCaptureWriter(maxBytes int) captureWriter {
	if maxBytes <= 0 {
		return &unboundedWriter{}
	}

	return &boundedWriter{max: maxBytes}
}

// unboundedWriter is a plain, uncapped capture.
type unboundedWriter struct {
	buf bytes.Buffer
}

func (w *unboundedWriter) Write(p []byte) (int, error) { return w.buf.Write(p) } //nolint:wrapcheck // bytes.Buffer.Write never errors

func (w *unboundedWriter) result() string { return w.buf.String() }

// boundedWriter caps retained bytes at max while still tracking every byte
// offered, so result can report the true overflow via a trailing marker
// matching internal/agent's truncateToolOutput format. It never errors or
// short-writes (io.Writer's contract, and what exec.Cmd's output copiers
// require): overflow bytes are silently discarded, not rejected.
type boundedWriter struct {
	buf   bytes.Buffer
	max   int
	total int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	n := len(p) // the real input length: io.Writer requires this back even when we retain less
	w.total += n

	if remaining := w.max - w.buf.Len(); remaining > 0 {
		if remaining < len(p) {
			p = p[:remaining]
		}

		w.buf.Write(p)
	}

	return n, nil
}

func (w *boundedWriter) result() string {
	s := w.buf.String()

	if overflow := w.total - w.buf.Len(); overflow > 0 {
		s += fmt.Sprintf("\n... [truncated %d bytes]", overflow)
	}

	return s
}

// HostRunner runs commands directly on the host via `sh -c`, with cwd as
// its working directory.
type HostRunner struct {
	cwd string
}

// wrapIfCanceled ensures err's chain satisfies errors.Is(_, ctx.Err()) when
// ctx was canceled by the time the command producing err finished. Go's
// os/exec only reliably surfaces a canceled context in cmd.Run's own
// returned error when the process happened to exit 0 despite cancellation
// (see exec.Cmd.Wait's watchCtx handling) — the far more common case (the
// process dies non-zero, or is killed by the cancellation itself) leaves no
// trace of *why* in Go's own error. Callers that need to tell "this command
// was interrupted by shutdown" apart from "this command just failed on its
// own" must go through this rather than trusting err alone. Only call this
// when err is already non-nil — it must never turn a successful command into
// a failure just because ctx happens to be canceled by the time it returns.
func wrapIfCanceled(ctx context.Context, err error) error {
	ctxErr := ctx.Err()
	if ctxErr == nil {
		return err
	}

	return fmt.Errorf("%w: %w", ctxErr, err)
}

// CanceledError returns a non-nil error wrapping ctx.Err() when ctx is
// canceled, or nil otherwise. RunCaptureFull/RunCaptureFullLimited
// deliberately report even a signal-killed process as data (a normal
// nonzero exit), not a Go error, so their own err return can't be used to
// detect "this result may be incomplete/unreliable because ctx was
// canceled while the command ran." A caller that needs that distinction —
// to tell a shutdown-interrupted step apart from a step that genuinely
// failed on its own — calls CanceledError immediately after such a call
// returns, instead.
func CanceledError(ctx context.Context) error {
	ctxErr := ctx.Err()
	if ctxErr == nil {
		return nil
	}

	return fmt.Errorf("context canceled: %w", ctxErr)
}

// exitCodeOf extracts a process's exit code from cmd.Run's error (0 for a
// nil error). A signal-killed process (e.g. one cut off by a context
// timeout) is also an *exec.ExitError, and Go reports its ExitCode() as -1 —
// the same sentinel a never-started process would need. So -1 here is
// ambiguous by itself; callers that must tell "ran but was killed" apart
// from "never started at all" use processStarted, not this return value.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}

	return -1
}

// processStarted reports whether cmd.Run's error indicates the process
// actually started — and then exited nonzero or was killed by a signal — as
// opposed to never starting at all (bad cwd, permission error, missing
// interpreter). Both cases can produce exitCodeOf(err) == -1, so this is the
// only reliable way to distinguish them.
func processStarted(err error) bool {
	if err == nil {
		return true
	}

	var exitErr *exec.ExitError

	return errors.As(err, &exitErr)
}

// Run runs command via `sh -c command` with h.cwd as its working directory,
// streaming stdout/stderr live to the terminal.
func (h HostRunner) Run(ctx context.Context, command string) error {
	slog.Debug("shell.run", "command", command, "cwd", h.cwd)

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // executing pipeline-defined commands is this tool's entire purpose
	cmd.Dir = h.cwd
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()

	slog.Debug("shell.run", "command", command, "cwd", h.cwd, "exit_code", exitCodeOf(err))

	if err != nil {
		return fmt.Errorf("command %q failed: %w", command, wrapIfCanceled(ctx, err))
	}

	return nil
}

// RunCapture runs command via `sh -c command` with h.cwd as its working
// directory, capturing stdout and stderr while also streaming stderr live.
// The captured output is logged (at debug level) on both success and
// failure, so a failing check/out command's output is available for
// debugging — previously it was discarded the moment the command exited
// nonzero, leaving only the terse "exit status N" from the wrapped error.
func (h HostRunner) RunCapture(ctx context.Context, command string) ([]byte, error) {
	slog.Debug("shell.capture", "command", command, "cwd", h.cwd)

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // executing pipeline-defined commands is this tool's entire purpose
	cmd.Dir = h.cwd
	cmd.Stdin = os.Stdin

	var outBuf, errBuf bytes.Buffer

	cmd.Stdout = &outBuf
	cmd.Stderr = io.MultiWriter(os.Stderr, &errBuf)

	err := cmd.Run()

	slog.Debug("shell.capture", "command", command, "cwd", h.cwd, "exit_code", exitCodeOf(err),
		"output_bytes", outBuf.Len(), "output", outBuf.String(), "stderr", errBuf.String())

	if err != nil {
		return nil, fmt.Errorf("command %q failed: %w", command, wrapIfCanceled(ctx, err))
	}

	return outBuf.Bytes(), nil
}

// RunCaptureFull runs command via `sh -c command` with h.cwd as its working
// directory, capturing stdout and stderr separately. Unlike Run/RunCapture
// (where any nonzero exit fails the step), a normal nonzero exit is reported
// as data via exitCode rather than a Go error — callers that need a
// command's failure to be observable data (e.g. an agent step's tool
// results) rather than a hard abort use this instead. Only a failure to
// start the process (not the process's own exit code, even a signal-killed
// one — e.g. from a canceled ctx) returns a non-nil error. A caller that
// needs to tell "this result may be incomplete because ctx was canceled
// while the command ran" apart from an ordinary exit code checks ctx.Err()
// itself (or CanceledError) after this returns, rather than relying on err —
// see runFixTask/runAssertedTask/evaluateStepGuard.
//
// Unlike Run/RunCapture, stdin is /dev/null, not the parent's: this runs
// non-interactive, model-generated commands, and inheriting an interactive
// stdin risks a command (cat with no args, a tool prompting for input)
// blocking until the step's timeout instead of getting EOF.
func (h HostRunner) RunCaptureFull(ctx context.Context, command string) (stdout, stderr string, exitCode int, err error) {
	return h.runCaptureFull(ctx, command, 0)
}

// RunCaptureFullLimited is RunCaptureFull with each stream capped at
// maxBytes while the command runs — see the Runner interface doc.
func (h HostRunner) RunCaptureFullLimited(ctx context.Context, command string, maxBytes int) (stdout, stderr string, exitCode int, err error) {
	return h.runCaptureFull(ctx, command, maxBytes)
}

// runCaptureFull is the shared implementation behind RunCaptureFull (maxBytes
// 0, meaning unbounded — byte-identical to before RunCaptureFullLimited
// existed) and RunCaptureFullLimited (maxBytes > 0).
func (h HostRunner) runCaptureFull(ctx context.Context, command string, maxBytes int) (stdout, stderr string, exitCode int, err error) {
	slog.Debug("shell.capture_full", "command", command, "cwd", h.cwd)

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // executing pipeline-defined commands is this tool's entire purpose
	cmd.Dir = h.cwd
	cmd.Stdin = nil

	outWriter := newCaptureWriter(maxBytes)
	errWriter := newCaptureWriter(maxBytes)

	cmd.Stdout = outWriter
	cmd.Stderr = errWriter

	runErr := cmd.Run()

	if !processStarted(runErr) {
		return "", "", -1, fmt.Errorf("command %q failed to start: %w", command, runErr)
	}

	code := exitCodeOf(runErr)

	slog.Debug("shell.capture_full", "command", command, "cwd", h.cwd, "exit_code", code)

	return outWriter.result(), errWriter.result(), code, nil
}

// IsExitError reports whether err (however wrapped) stems from a process that
// started and then exited nonzero or was killed by a signal — as opposed to an
// infrastructure failure to run it at all. Callers use it to tell a task-level
// failure (a command's own nonzero exit) apart from an errored one so hooks
// dispatch correctly; a resource check's failure, by contrast, is left
// unwrapped so it classifies as errored.
func IsExitError(err error) bool {
	var exitErr *exec.ExitError

	return errors.As(err, &exitErr)
}

// RunShell runs command on the host via `sh -c command` with cwd as its
// working directory, streaming stdout/stderr live. Equivalent to
// HostRunner{cwd: cwd}.Run; kept as a package-level function for callers
// that never need containerized execution.
func RunShell(ctx context.Context, command, cwd string) error {
	return HostRunner{cwd: cwd}.Run(ctx, command)
}

// RunShellCapture runs command on the host via `sh -c command`, capturing
// stdout. Equivalent to HostRunner{cwd: cwd}.RunCapture.
func RunShellCapture(ctx context.Context, command, cwd string) ([]byte, error) {
	return HostRunner{cwd: cwd}.RunCapture(ctx, command)
}

// RunShellCaptureFull runs command on the host via `sh -c command`,
// capturing stdout/stderr separately and reporting a normal nonzero exit as
// data. Equivalent to HostRunner{cwd: cwd}.RunCaptureFull.
func RunShellCaptureFull(ctx context.Context, command, cwd string) (stdout, stderr string, exitCode int, err error) {
	return HostRunner{cwd: cwd}.RunCaptureFull(ctx, command)
}
