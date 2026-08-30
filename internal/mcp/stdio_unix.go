//go:build unix

package mcp

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts a stdio server in its own process group and makes
// cancellation kill that group rather than just its leader.
//
// exec.CommandContext, and the SDK's own SIGTERM/SIGKILL escalation, both
// signal cmd.Process alone. An MCP server that forks — npx starting node,
// gopls starting `go` — is not the process that needs to die, or not the only
// one: its children survive, and under `steps web`, which reconnects on
// every poll, that is unbounded process-table growth from an otherwise
// idle-looking pipeline.
//
// It is also why stdioWaitDelay has to exist at all. A surviving grandchild
// inherits the stderr pipe, so cmd.Wait blocks on a copy that never sees EOF;
// the delay bounds that wait instead of removing its cause. Killing the group
// removes the cause, and leaves the delay as the backstop it was meant to be.
//
// This mirrors the fix internal/shell's cancelWaitDelay comment describes but
// did not take, for the same reason it named: it is unix-only and wants a
// build-tagged file. This is that file.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Replaces exec.CommandContext's default, which is cmd.Process.Kill().
	// Negating the pid addresses the whole group; the child is its leader, so
	// its pid IS the group id. ESRCH here means the group is already gone,
	// which is the outcome being asked for.
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err != nil {
			return cmd.Process.Kill()
		}

		return nil
	}
}
