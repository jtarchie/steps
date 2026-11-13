package shell

// Running a command below a runner's own directory without a second session.

import (
	"errors"
	"fmt"
)

// subdirRunner is a runner that can work one directory down and still share its session.
type subdirRunner interface {
	InSubdir(rel string) Runner
}

// InSubdir is runner with its commands run in rel below its own directory, on the same tree: a fix: agent's dir: works inside the failing task's container, since a second container would be a second copy of the tree under volumes (steps#246).
func InSubdir(runner Runner, rel string) (Runner, error) {
	if rel == "" || rel == "." {
		return runner, nil
	}

	sub, ok := runner.(subdirRunner)
	if !ok {
		return nil, fmt.Errorf("%w: %T", errNoSubdir, runner)
	}

	return sub.InSubdir(rel), nil
}

var errNoSubdir = errors.New("this runner cannot run a command below its own directory")
