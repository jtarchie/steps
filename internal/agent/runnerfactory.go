package agent

// Who builds the runner an agent's shell-backed tools use.

import (
	"context"

	"github.com/jtarchie/steps/internal/shell"
)

// RunnerFactory builds the runner a step's run_shell, custom tools and containerized file tools all execute through.
//
// It is a parameter rather than a call to shell.NewRunner because placing a step is internal/pipeline's knowledge and not this package's: pipeline is what turns a step's tags: into a machine, and the dependency graph deliberately keeps internal/venue out of here. A nil factory runs on this machine, which is what every caller with no placement to apply passes.
type RunnerFactory func(ctx context.Context, spec shell.RunnerSpec) (shell.Runner, error)

// build is the factory with its own default folded in, so a nil one is a legal value everywhere rather than something each caller has to guard.
func (f RunnerFactory) build(ctx context.Context, spec shell.RunnerSpec) (shell.Runner, error) {
	if f == nil {
		return shell.NewRunner(spec) //nolint:wrapcheck // the caller names the agent
	}

	return f(ctx, spec)
}
