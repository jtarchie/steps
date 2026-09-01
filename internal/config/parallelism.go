package config

// parallelism: — N concurrent shards of one task, as across: sugar.

import (
	"errors"
	"fmt"
	"strconv"
)

// Sharded reports whether this step began as parallelism: — the marker the
// desugared matrix keeps so the count var and the run report can speak the
// author's word. See the field's comment in step.go.
func (s Step) Sharded() bool {
	return s.Parallelism != nil && *s.Parallelism > 0
}

// desugarParallelism rewrites every parallelism: step into the across: matrix
// it abbreviates.
//
// It runs BEFORE validate(), and that ordering carries the design: the axis
// it writes is valid by construction, and the across validators then apply to
// the desugared step exactly as they would to the hand-written form — collect
// rules, budget:, max_in_flight:, the hook rejection — so the sugar cannot
// drift from the mechanism it abbreviates. Its own rejections are worded as
// parallelism:, since that is what the author wrote; Load joins them with
// validate()'s, so one load still names every mistake.
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
	if step.Parallelism == nil {
		return nil
	}

	// A pointer, like Attempts and for the same reason: an explicit 0 is a
	// reasonable guess at the zero-means-no-limit convention the other dials
	// follow, and here it would silently mean "sugar off" — one unsharded run
	// with the never-rendered {{ .vars.index }} text handed to the shell.
	shards := *step.Parallelism
	if shards <= 0 {
		return fmt.Errorf("%s: parallelism is %d; it counts shards, so it must be positive", label, shards)
	}

	// The same ceiling every matrix width has (from_file: axes are capped at
	// dispatch): a hand-written values: list is self-limiting YAML, but one
	// mistyped digit here would expand at load — and make(N) at int64 scale
	// panics before any error could say why.
	if shards > MaxAcrossItems {
		return fmt.Errorf("%s: parallelism is %d, above the limit of %d; split the work across jobs if one command truly needs more shards", label, shards, MaxAcrossItems)
	}

	if len(step.Across) > 0 {
		return fmt.Errorf("%s: parallelism: beside across: is ambiguous; declare the shard axis yourself — across: [{var: ..., values: [...]}]", label)
	}

	if step.Task == "" {
		// across: on a try: wrapper renders through to the task it wraps, so
		// the near-miss deserves a pointer rather than a message asserting a
		// visible task step is not one.
		if step.Try != nil && unwrapStep(step).Task != "" {
			return fmt.Errorf("%s: parallelism: belongs on the task inside the try:, not on the try: wrapper", label)
		}

		return fmt.Errorf("%s: parallelism: is only valid on a task step; it runs N copies of one command", label)
	}

	values := make([]string, shards)
	for i := range values {
		values[i] = strconv.Itoa(i + 1)
	}

	step.Across = []AcrossVar{{Var: "index", Values: values}}

	// Concurrent by default — the name is the semantics. The matrix default
	// is serial because hand-written cells often mean to run in order; N
	// copies of one command have no order to mean. An authored value wins —
	// including max_in_flight: 0, which this cannot distinguish from unset
	// and which therefore means N here, never serial (docs/control-flow.md
	// says so where the sugar is described).
	if step.MaxInFlight == 0 {
		step.MaxInFlight = shards
	}

	return nil
}
