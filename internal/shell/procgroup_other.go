//go:build !unix

package shell

import (
	"context"
	"os/exec"
)

// Start has nothing to isolate off unix: no sessions, no terminal stop signals.
func Start(_ context.Context, cmd *exec.Cmd) (release func(), err error) {
	return func() {}, cmd.Start() //nolint:wrapcheck // the caller names the command
}

// KillAll has nothing to kill: Start made no sessions.
func KillAll() {}

// IgnoreTerminalStops is a no-op: there are no terminal stop signals off unix.
func IgnoreTerminalStops() {}
