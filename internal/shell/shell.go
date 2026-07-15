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

// HostRunner runs commands directly on the host via `sh -c`, with cwd as
// its working directory.
type HostRunner struct {
	cwd string
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
		return fmt.Errorf("command %q failed: %w", command, err)
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
		return nil, fmt.Errorf("command %q failed: %w", command, err)
	}

	return outBuf.Bytes(), nil
}

// RunCaptureFull runs command via `sh -c command` with h.cwd as its working
// directory, capturing stdout and stderr separately. Unlike Run/RunCapture
// (where any nonzero exit fails the step), a normal nonzero exit is reported
// as data via exitCode rather than a Go error — callers that need a
// command's failure to be observable data (e.g. an agent step's tool
// results) rather than a hard abort use this instead. Only a failure to
// start the process (not the process's own exit code) returns a non-nil
// error.
//
// Unlike Run/RunCapture, stdin is /dev/null, not the parent's: this runs
// non-interactive, model-generated commands, and inheriting an interactive
// stdin risks a command (cat with no args, a tool prompting for input)
// blocking until the step's timeout instead of getting EOF.
func (h HostRunner) RunCaptureFull(ctx context.Context, command string) (stdout, stderr string, exitCode int, err error) {
	slog.Debug("shell.capture_full", "command", command, "cwd", h.cwd)

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // executing pipeline-defined commands is this tool's entire purpose
	cmd.Dir = h.cwd
	cmd.Stdin = nil

	var outBuf, errBuf bytes.Buffer

	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()

	if !processStarted(runErr) {
		return "", "", -1, fmt.Errorf("command %q failed to start: %w", command, runErr)
	}

	code := exitCodeOf(runErr)

	slog.Debug("shell.capture_full", "command", command, "cwd", h.cwd, "exit_code", code)

	return outBuf.String(), errBuf.String(), code, nil
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
