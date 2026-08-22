package venue

// The decision one tier above shell's.

import (
	"fmt"
	"os"
	"strings"

	"github.com/jtarchie/steps/internal/shell"
)

// NewRunner returns a runner for spec, on a worker when spec.Worker names one
// and on this machine otherwise.
//
// The empty case hands straight back to shell.NewRunner, unchanged and
// undecorated. That is deliberate: placement is opt-in, and a pipeline that
// never mentions a worker must take exactly the path it took before this
// package existed — same runner, same behavior, nothing wrapped.
func NewRunner(spec shell.RunnerSpec) (shell.Runner, error) {
	if spec.Worker == "" {
		return shell.NewRunner(spec) //nolint:wrapcheck // the local path is shell's answer, returned as shell phrased it
	}

	worker, err := ParseWorker(spec.Worker)
	if err != nil {
		return nil, err
	}

	if spec.Image != "" {
		// Load-time validation refuses this combination, so reaching it means
		// a caller built a spec by hand. Saying so beats silently running the
		// command outside the container it asked for.
		return nil, fmt.Errorf("%w %q: a step cannot name both a worker and an image", ErrWorker, spec.Worker)
	}

	return runner{session: &session{
		worker:  worker,
		cwd:     spec.Cwd,
		outputs: spec.Fetch,
		env:     resolveEnv(spec.Env),
		keep:    keepWorkspace(),
	}}, nil
}

// resolveEnv reads the values of the variables a pipeline's env: opted into.
//
// Resolved here, on the orchestrator, because that is where the operator's
// environment is. A name that is not set contributes nothing rather than an
// empty value — the same rule the host path follows, and for the same reason:
// the two are different to a command that tests for presence, and inventing an
// empty one turns a forgotten export into a silent misconfiguration.
func resolveEnv(names []string) map[string]string {
	if len(names) == 0 {
		return nil
	}

	values := make(map[string]string, len(names))

	for _, name := range names {
		value, ok := os.LookupEnv(name)
		if ok {
			values[name] = value
		}
	}

	return values
}

// keepWorkspace reports whether --keep-workspace is in effect, so a worker
// leaves its scratch behind too.
//
// Read from the environment rather than threaded through RunnerSpec because
// that is where the flag already lives (STEPS_KEEP_WORKSPACE), and because the
// postmortem the flag exists for is worth exactly as much on the machine that
// ran the step as on the one that scheduled it — stopping at the machine
// boundary would be the same bug the flag was added to fix.
func keepWorkspace() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("STEPS_KEEP_WORKSPACE")))

	return value == "1" || value == "true" || value == "yes"
}
