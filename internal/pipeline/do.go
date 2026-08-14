package pipeline

// do: — several steps run one after another, as a single plan step.

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
)

// runDoStep runs a do: block's steps in declaration order, stopping at the
// first one that does not succeed, and reports the block's outcome as one
// step's.
//
// Sequential execution is what the surrounding plan does anyway — the point is
// the CONTAINMENT. The block is a single plan-positioned step, so a hook on it
// observes the whole group's outcome, which is the only way to say "roll back
// if any of these three failed" without repeating the hook on every step or
// hoisting it to the job.
//
// Stopping at the first failure is the sequential analogue of what a plan
// already does, and deliberately unlike across:, which runs every cell. A
// matrix asks "which of these combinations work"; a do: block is one piece of
// work spelled in several steps, and running the deploy after the migration
// failed is not a partial answer, it is a worse outcome.
func runDoStep(ctx context.Context, r stepRunner, i int, step config.Step, parentHash string) (stepResult, error) {
	content, err := merkle.DoNodeContent(r.cfg, step)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (do): %w", i, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindDo, content, parentHash)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (do): %w", i, err)
	}

	fmt.Printf("do: %d steps\n", len(step.Do))

	// Children are dispatched against the BLOCK's hash as their parent, so the
	// chain reads do -> child -> child rather than every child hanging off the
	// step before the block. That keeps a child's identity dependent on the
	// block it sits in, which is what makes moving a step into or out of a do:
	// a real change rather than an invisible one.
	childParent := hash

	for childIndex := range step.Do {
		child := step.Do[childIndex]

		// runNonGetStep rather than dispatchNonGetStep: a child is a real step,
		// so it gets its own when: guard, hooks, recorded execution and
		// published events, exactly as it would in the plan. The block adds
		// containment, not a different kind of execution.
		//
		// The BLOCK's plan index is passed, not the child's position, matching
		// every other block runner (see recordCompletedStep's doc, which states
		// the convention). A child's position is not a plan index: passing it
		// would publish this block's events under indices belonging to the
		// unrelated plan steps that really do sit at 0, 1, 2 — and the run
		// transcript would attribute a do: child's output to whichever step
		// happened to share its number.
		childRes, childErr := runNonGetStep(ctx, r, i, child, nil, childParent)

		// A try: child tolerates its own failure here, for the reason a try:
		// branch does inside a concurrent block (see runBranches): the plan
		// walk never sees a step nested in a block, and this is as far up as
		// one goes. Without it the same wrapper would be tolerated in a plain
		// plan and propagate inside a do:, which is the case try: is most
		// often reached for.
		childErr = tolerateTryFailure(ctx, r.jobName, child, childErr)
		if childErr != nil {
			return ran(hash), childErr
		}

		if childRes.hash != "" {
			childParent = childRes.hash
		}
	}

	return ran(hash), nil
}
