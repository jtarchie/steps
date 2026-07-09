package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// RunShell runs command via `sh -c command` with cwd as its working
// directory, streaming stdout/stderr live to the terminal.
func RunShell(ctx context.Context, command, cwd string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = cwd
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("command %q failed: %w", command, err)
	}

	return nil
}

// RunShellCapture runs command via `sh -c command` with cwd as its working
// directory, capturing and returning stdout while streaming stderr live.
func RunShellCapture(ctx context.Context, command, cwd string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = cwd
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("command %q failed: %w", command, err)
	}

	return out, nil
}
