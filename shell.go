package main

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

// exitCodeOf extracts a process's exit code from cmd.Run's error (0 for a
// nil error). -1 means the process never started at all (e.g. a bad cwd or
// permission error) rather than exiting nonzero — callers that care about
// that distinction check for it explicitly.
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

// RunShell runs command via `sh -c command` with cwd as its working
// directory, streaming stdout/stderr live to the terminal.
func RunShell(ctx context.Context, command, cwd string) error {
	slog.Debug("shell.run", "command", command, "cwd", cwd)

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // executing pipeline-defined commands is this tool's entire purpose
	cmd.Dir = cwd
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()

	slog.Debug("shell.run", "command", command, "cwd", cwd, "exit_code", exitCodeOf(err))

	if err != nil {
		return fmt.Errorf("command %q failed: %w", command, err)
	}

	return nil
}

// RunShellCapture runs command via `sh -c command` with cwd as its working
// directory, capturing stdout and stderr while also streaming stderr live.
// The captured output is logged (at debug level) on both success and
// failure, so a failing check/out command's output is available for
// debugging — previously it was discarded the moment the command exited
// nonzero, leaving only the terse "exit status N" from the wrapped error.
func RunShellCapture(ctx context.Context, command, cwd string) ([]byte, error) {
	slog.Debug("shell.capture", "command", command, "cwd", cwd)

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // executing pipeline-defined commands is this tool's entire purpose
	cmd.Dir = cwd
	cmd.Stdin = os.Stdin

	var outBuf, errBuf bytes.Buffer

	cmd.Stdout = &outBuf
	cmd.Stderr = io.MultiWriter(os.Stderr, &errBuf)

	err := cmd.Run()

	slog.Debug("shell.capture", "command", command, "cwd", cwd, "exit_code", exitCodeOf(err),
		"output_bytes", outBuf.Len(), "output", outBuf.String(), "stderr", errBuf.String())

	if err != nil {
		return nil, fmt.Errorf("command %q failed: %w", command, err)
	}

	return outBuf.Bytes(), nil
}

// RunShellCaptureFull runs command via `sh -c command` with cwd as its
// working directory, capturing stdout and stderr separately. Unlike
// RunShell/RunShellCapture (where any nonzero exit fails the step), a normal
// nonzero exit is reported as data via exitCode rather than a Go error —
// callers that need a command's failure to be observable data (e.g. an
// agent step's tool results) rather than a hard abort use this instead. Only
// a failure to start the process (not the process's own exit code) returns
// a non-nil error.
//
// Unlike RunShell/RunShellCapture, stdin is /dev/null, not the parent's:
// this runs non-interactive, model-generated commands, and inheriting an
// interactive stdin risks a command (cat with no args, a tool prompting for
// input) blocking until the step's timeout instead of getting EOF.
func RunShellCaptureFull(ctx context.Context, command, cwd string) (stdout, stderr string, exitCode int, err error) {
	slog.Debug("shell.capture_full", "command", command, "cwd", cwd)

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // executing pipeline-defined commands is this tool's entire purpose
	cmd.Dir = cwd
	cmd.Stdin = nil

	var outBuf, errBuf bytes.Buffer

	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()

	code := exitCodeOf(runErr)
	if runErr != nil && code == -1 {
		return "", "", -1, fmt.Errorf("command %q failed to start: %w", command, runErr)
	}

	slog.Debug("shell.capture_full", "command", command, "cwd", cwd, "exit_code", code)

	return outBuf.String(), errBuf.String(), code, nil
}
