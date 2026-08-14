//go:build !unix

package mcp

import "os/exec"

// setProcessGroup is a no-op where process groups are not a thing this can
// portably ask for. exec.CommandContext's default cancellation (kill the
// direct child) stands, and stdioWaitDelay remains the only bound on a
// grandchild that outlives it — see the unix implementation for what is
// being given up.
func setProcessGroup(*exec.Cmd) {}
