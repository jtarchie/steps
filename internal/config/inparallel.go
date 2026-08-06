package config

// The in_parallel: step — several steps running at the same time.

import "fmt"

// InParallel runs its steps concurrently rather than in sequence.
//
// A plan is otherwise strictly sequential: three independent downloads, or a
// lint task and a test task, wait on each other for no reason at all, and one
// slow resource check stalls everything behind it.
type InParallel struct {
	// Steps are the branches, each an ordinary step. Empty is a load error:
	// an in_parallel: with nothing in it is a typo, and running zero steps
	// "successfully" is the least useful reading of it.
	Steps []Step `yaml:"steps"`
	// Limit caps how many branches are in flight at once. 0 (the default) is
	// unbounded — the plan author asked for parallelism, so making them also
	// ask for a width would be a second decision for the common case.
	Limit int `yaml:"limit,omitempty"`
	// FailFast, when true, cancels the remaining branches as soon as one
	// fails. A pointer so unset is distinguishable from an explicit false.
	//
	// Either way the block FAILS when a branch does. fail_fast decides
	// whether the siblings are given a chance to finish, never whether the
	// failure counts — a swallowed failure was a real defect in the first
	// implementation of this step, and it is what assert.outcome exists to
	// catch (see docs/control-flow.md).
	FailFast *bool `yaml:"fail_fast,omitempty"`
}

// FailsFast reports whether a branch failure cancels its siblings. Absent
// means false: letting the others finish is the less surprising default, and
// it is what makes the failure report complete rather than truncated at
// whichever branch happened to fail first.
func (p *InParallel) FailsFast() bool {
	return p != nil && p.FailFast != nil && *p.FailFast
}

// validateInParallel enforces the shape of an in_parallel: block and the
// fields that have no meaning on one.
//
// The list is long because every entry is a claim a reader would otherwise
// have to discover by running it. An in_parallel: block is a container, not an
// operation: it fetches nothing, runs nothing, produces nothing of its own, and
// has no outcome to route on beyond its branches' — so every field that
// describes an operation is rejected on it.
func (c *Config) validateInParallel() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.InParallel == nil {
				return nil
			}

			return c.validateInParallelBlock(label, step)
		})
		if err != nil {
			return err
		}
	}

	return c.validateParallelOutputs()
}

func (c *Config) validateInParallelBlock(label string, step *Step) error {
	if len(step.InParallel.Steps) == 0 {
		return fmt.Errorf("%s: in_parallel.steps must not be empty", label)
	}

	if step.InParallel.Limit < 0 {
		return fmt.Errorf("%s: in_parallel.limit must not be negative (omit it for unbounded)", label)
	}

	return c.rejectOperationFields(label, step, "an in_parallel")
}

// validateParallelOutputs rejects two branches of one in_parallel: block
// producing the same output name.
//
// Sequential steps may reuse an output name — a later one simply replaces what
// an earlier one wrote, in a defined order. Concurrent ones have no order, so
// the same name written by two branches is a race whose winner is whichever
// finished last, which is not something a pipeline can mean. Nested blocks are
// walked too: the first implementation of this step checked only the immediate
// children, and a duplicate one level down went undetected.
func (c *Config) validateParallelOutputs() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.InParallel == nil {
				return nil
			}

			seen := map[string]bool{}

			for i := range step.InParallel.Steps {
				for _, name := range branchOutputs(&step.InParallel.Steps[i]) {
					if seen[name] {
						return fmt.Errorf("%s: duplicate output names across in_parallel branches: %q is produced by more than one branch, and concurrent branches have no order to resolve that with",
							label, name)
					}

					seen[name] = true
				}
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// branchOutputs lists every artifact one branch produces, looking through a
// try: wrapper and down into a nested block — a duplicate is a duplicate
// however deeply the branch that produces it is nested.
//
// A race: block contributes its branches' outputs once, not once per branch:
// every racer declares the same outputs by construction (validateRaceBlock),
// and only the winner's are produced.
func branchOutputs(step *Step) []string {
	if step.Try != nil {
		return branchOutputs(step.Try)
	}

	if step.Race != nil {
		// Every racer declares the same outputs (validateRaceBlock enforces
		// it), so one branch speaks for all of them. The length check is not
		// defensive noise: validate() runs every checker and joins their
		// errors, so this walk reaches a malformed race BEFORE validateRace
		// can reject it — indexing [0] here panicked on an empty race nested
		// in an in_parallel:, taking down `steps watch` on a config typo.
		if len(step.Race.Steps) == 0 {
			return nil
		}

		return branchOutputs(&step.Race.Steps[0])
	}

	if step.InParallel == nil {
		return step.Outputs
	}

	var outputs []string

	for i := range step.InParallel.Steps {
		outputs = append(outputs, branchOutputs(&step.InParallel.Steps[i])...)
	}

	return outputs
}
