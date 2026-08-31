package config

// parallelism: — N concurrent shards of one task, as across: sugar.

import (
	"errors"
	"fmt"
	"strconv"
)

// desugarParallelism rewrites every parallelism: step into the across: matrix
// it abbreviates.
//
// It runs BEFORE validate(), and that ordering carries the design: the axis
// it writes is valid by construction, and the across validators then apply to
// the desugared step exactly as they would to the hand-written form — collect
// rules, budget:, max_in_flight: — so the sugar cannot drift from the
// mechanism it abbreviates. Its own rejections are worded as parallelism:,
// since that is what the author wrote.
func (c *Config) desugarParallelism() error {
	var failures []error

	for _, job := range c.Jobs {
		// The walk never stops: a pipeline with a misplaced parallelism: in
		// two jobs should take one load to name both, matching validate()'s
		// joined-errors contract.
		_ = job.visitSteps(func(label string, step *Step) error {
			err := desugarStepParallelism(label, step)
			if err != nil {
				failures = append(failures, err)
			}

			return nil
		})
	}

	return errors.Join(failures...)
}

func desugarStepParallelism(label string, step *Step) error {
	if step.Parallelism == 0 {
		return nil
	}

	if step.Parallelism < 0 {
		return fmt.Errorf("%s: parallelism is %d; it counts shards, so it must be positive", label, step.Parallelism)
	}

	if len(step.Across) > 0 {
		return fmt.Errorf("%s: parallelism: beside across: is ambiguous; declare the shard axis yourself — across: [{var: ..., values: [...]}]", label)
	}

	if step.Task == "" {
		return fmt.Errorf("%s: parallelism: is only valid on a task step; it runs N copies of one command", label)
	}

	values := make([]string, step.Parallelism)
	for i := range values {
		values[i] = strconv.Itoa(i + 1)
	}

	step.Across = []AcrossVar{{Var: "index", Values: values}}

	// Concurrent by default — the name is the semantics. The matrix default
	// is serial because hand-written cells often mean to run in order; N
	// copies of one command have no order to mean. An authored value narrows.
	if step.MaxInFlight == 0 {
		step.MaxInFlight = step.Parallelism
	}

	return nil
}
