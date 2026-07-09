package main

import (
	"context"
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
		err = fmt.Errorf("command %q failed: %w", command, err)
		slog.Error("shell.run", "command", command, "cwd", cwd, "error", err)

		return err
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
		err = fmt.Errorf("command %q failed: %w", command, err)
		slog.Error("shell.capture", "command", command, "cwd", cwd, "error", err)

		return nil, err
	}

	slog.Debug("shell.capture", "command", command, "cwd", cwd, "output_bytes", len(out), "output", string(out))

	return out, nil
}
