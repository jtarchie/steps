package venue

// The decision one tier above shell's.

import (
	"errors"
	"os"

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
		// Refused rather than dropped: a local runner cannot fetch them, so
		// the step would run against a tree missing those inputs.
		if len(spec.RemoteInputs) > 0 {
			return nil, errRemoteInputsHere
		}

		return shell.NewRunner(spec) //nolint:wrapcheck // the local path is shell's answer, returned as shell phrased it
	}

	worker, err := ParseWorker(spec.Worker)
	if err != nil {
		return nil, err
	}

	if worker.dockerPlus() {
		return newPlusRunner(worker, spec)
	}

	return newBareRunner(worker, spec)
}

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
