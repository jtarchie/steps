package pipeline

// do: — several steps run one after another, as a single plan step.

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
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
func runDoStep(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step,
	bw workspace.BuildWorkspace, st *store.Store, parentHash string, handoff *agent.Handoff,
) (string, stepDisposition, nonGetOutcome, error) {
	content, err := merkle.DoNodeContent(cfg, step)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (do): %w", i, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindDo, content, parentHash)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (do): %w", i, err)
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
		childHash, _, _, childErr := runNonGetStep(
			ctx, cfg, jobName, childIndex, child, bw, st, nil, childParent, handoff)

		// A try: child tolerates its own failure here, for the reason a try:
		// branch does inside a concurrent block (see runBranches): the plan
		// walk never sees a step nested in a block, and this is as far up as
		// one goes. Without it the same wrapper would be tolerated in a plain
		// plan and propagate inside a do:, which is the case try: is most
		// often reached for.
		childErr = tolerateTryFailure(ctx, jobName, child, childErr)
		if childErr != nil {
			return hash, stepRan, nonGetOutcome{}, childErr
		}

		if childHash != "" {
			childParent = childHash
		}
	}

	return hash, stepRan, nonGetOutcome{}, nil
}
