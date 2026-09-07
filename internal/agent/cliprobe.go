package agent

// Preflight for a CLI target: is the binary there?

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// probeCLI answers preflight's question for a CLI target: is the binary
// there?
//
// That is deliberately all it asks. The HTTP probe sends a real request
// because an endpoint can be reachable and still reject the model or the
// key; a host CLI has no equivalent failure that a cheap check would catch,
// and spawning one to find out would put a process launch in the path of
// every `steps web` poll. A CLI that is installed but broken fails at the
// step, with the CLI's own error, which is a better message than a probe
// would synthesize.
//
// The CLI is always a host subprocess now (see the issue #100 design note in
// cliexec.go), whether or not the step names an image: for its tools, so
// there is no containerized case left to probe differently — image: no
// longer changes what question this asks.
func probeCLI(_ context.Context, ri config.ResolvedInvocation, _ time.Duration) error {
	binary := config.CLIBinary(ri.CLI)
	if binary == "" {
		return fmt.Errorf("agent %q: no runtime for cli %q", ri.AgentName, ri.CLI)
	}

	_, err := exec.LookPath(binary)
	if err != nil {
		return fmt.Errorf("agent %q: cli %q is not on PATH: %w", ri.AgentName, binary, err)
	}

	return nil
}
