package venue

// The decision one tier above shell's.

import (
	"errors"
	"os"

	"github.com/jtarchie/steps/internal/shell"
)

// NewRunner returns a runner for spec on worker, which is dialed exactly as handed in: the struct a lease resolved, never a URL parsed back from text. The zero Worker means this machine.
//
// The zero case hands back to shell.NewRunner, except a container step with a tree: that runs on the implicit local: worker, through volumes, so this machine's docker is reached the way any other is and never bind-mounts a host path (steps#246).
func NewRunner(worker Worker, spec shell.RunnerSpec) (shell.Runner, error) {
	if worker.Here() {
		// Refused rather than dropped: a local runner cannot fetch them, so
		// the step would run against a tree missing those inputs.
		if len(spec.RemoteInputs) > 0 {
			return nil, errRemoteInputsHere
		}

		// A tree-less command (a check, an export call) has no path to mount, and a holder container per poll would buy nothing.
		if spec.Image != "" && spec.Cwd != "" {
			return newPlusRunner(implicitLocal, spec)
		}

		return shell.NewRunner(spec) //nolint:wrapcheck // the local path is shell's answer, returned as shell phrased it
	}

	if worker.dockerPlus() {
		return newPlusRunner(worker, spec)
	}

	return newBareRunner(worker, spec)
}

// implicitLocal is the worker an untagged container step runs on: the same cache namespace as an explicit bare local:.
var implicitLocal = Worker{URL: "local:", Scheme: SchemeLocal} //nolint:gochecknoglobals // a constant struct

// errRemoteInputsHere is a spec naming inputs other workers hold with no
// worker to offer them to.
var errRemoteInputsHere = errors.New("inputs held on other workers need a worker to receive them, and this step has none")

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

// WorkerEnv is the variable a placed command can read to learn where it is.
//
// It exists because placement was otherwise entirely invisible from inside a
// step: a script that needs to behave differently on a worker — or a person
// reading a log wondering whether the tag took effect — had nothing to ask.
// Absent, rather than empty, for a step running on this machine, so the two
// are distinguishable to a command that tests for presence.
const WorkerEnv = "STEPS_WORKER"

func withWorkerTag(env map[string]string, tag string) map[string]string {
	if tag == "" {
		return env
	}

	if env == nil {
		env = map[string]string{}
	}

	env[WorkerEnv] = tag

	return env
}
