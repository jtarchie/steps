package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
)

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
	if err != nil {
		return fmt.Errorf("command %q failed: %w", command, err)
	}

	slog.Debug("shell.run", "command", command, "cwd", cwd, "exit_code", 0)

	return nil
}

// RunShellCapture runs command via `sh -c command` with cwd as its working
// directory, capturing and returning stdout while streaming stderr live.
func RunShellCapture(ctx context.Context, command, cwd string) ([]byte, error) {
	slog.Debug("shell.capture", "command", command, "cwd", cwd)

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // executing pipeline-defined commands is this tool's entire purpose
	cmd.Dir = cwd
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("command %q failed: %w", command, err)
	}

	slog.Debug("shell.capture", "command", command, "cwd", cwd, "output_bytes", len(out), "output", string(out))

	return out, nil
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
	if runErr != nil {
		var exitErr *exec.ExitError

		if errors.As(runErr, &exitErr) {
			slog.Debug("shell.capture_full", "command", command, "cwd", cwd, "exit_code", exitErr.ExitCode())

			return outBuf.String(), errBuf.String(), exitErr.ExitCode(), nil
		}

		return "", "", -1, fmt.Errorf("command %q failed to start: %w", command, runErr)
	}

	slog.Debug("shell.capture_full", "command", command, "cwd", cwd, "exit_code", 0)

	return outBuf.String(), errBuf.String(), 0, nil
}
