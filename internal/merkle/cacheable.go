package merkle

import (
	"fmt"

	"github.com/jtarchie/steps/internal/config"
)

// CellHash is one across: cell's own content hash, plus whether the cell is
// the kind of step that may be skipped when it matches.
//
// A put or an agent cell is never cacheable, for the same reason a put or
// agent step never is: side effects, and non-determinism. Nor is a task cell
// that produces outputs: a skipped cell captures nothing, so a rerun would
// hand the step consuming the collected artifact a directory the skipped
// cell's contribution is simply missing from — and the store keeps no
// artifact bytes to replay. Re-running is the only honest answer.
func CellHash(cfg *config.Config, cell config.Step, parentHash string) (string, bool, error) {
	content, err := stepContentMap(cfg, cell)
	if err != nil {
		return "", false, fmt.Errorf("across cell: %w", err)
	}

	kind, _ := cell.Kind()

	hash, err := HashNode(NodeKind(kind), content, parentHash)
	if err != nil {
		return "", false, fmt.Errorf("across cell: %w", err)
	}

	cacheable := kind == config.StepKindTask && cell.Fix == nil && cell.When == nil && cell.To == nil &&
		!taskCellProducesOutputs(cfg, cell)

	return hash, cacheable, nil
}

// StepCacheable reports whether a step's outputs may be reused from an earlier
// run that did the same work over the same input bytes — the per-step cache in
// internal/workspace, which is a different mechanism from the chain skip the
// rest of this package feeds.
//
// The two answer different questions. A chain skip asks "did this whole
// remaining plan already succeed, unchanged", and ENDS the walk, so nothing
// downstream ever observes an artifact it did not produce. A step cache hit
// lets the plan continue, which is what makes the exclusions below necessary
// rather than merely cautious: a hit restores declared outputs and nothing
// else, so a step whose result is anything MORE than its outputs must run.
//
//   - volatile: is the author saying this step reads something undeclared.
//   - A verdict or a to: is a decision the store records and routing keys on.
//     Replaying it means writing "decided X, reused from run N" into the run
//     record — a separate feature with its own audit questions.
//   - context: from: reads an upstream decision delivered at run time, which
//     the action key cannot see. A hit would reuse an answer computed from a
//     decision that has since changed.
//   - Hooks are side effects the author asked for by name, and no kind of skip
//     fires them (see internal/pipeline's runNonGetStep).
//   - A fix: agent runs only when the command failed, so a cache entry records
//     precisely the case that never needed it.
//   - An across: cell captures into per-coordinate subdirectories that mean
//     something only inside the matrix that composed them.
//   - A step declaring NO outputs: has nothing for a hit to restore, so
//     reusing it would not be reuse — it would be omission. Whatever such a
//     step is for is either its exit code as a gate or a side effect, and
//     dropping a side effect silently is the failure put: is excluded to
//     avoid. Caching a gate is a real thing to want and a different feature,
//     with its own opt-in to design.
//
// when: is deliberately NOT excluded. It decides whether the step runs at all,
// and is evaluated before the cache is consulted: a false guard skips the step
// without a lookup, and a true one leaves work whose result is identified by
// its inputs like any other.
func StepCacheable(cfg *config.Config, step config.Step) bool {
	kind, ok := step.Kind()
	if !ok || !stepRecordsOnlyOutputs(step) {
		return false
	}

	switch kind { //nolint:exhaustive // every other kind is rejected below, which is what the default is for
	case config.StepKindAgent:
		return len(step.Outputs) > 0
	case config.StepKindTask:
		// Conservative on a resolution error, matching taskCellProducesOutputs:
		// a step that cannot be resolved must not be reported cacheable.
		rt, err := cfg.ResolveTask(step)

		return err == nil && rt.Fix == nil && len(rt.Outputs) > 0
	default:
		return false
	}
}

// stepRecordsOnlyOutputs reports that a step's whole observable result is the
// artifacts it declares — nothing routed on, read downstream, or reacted to by
// a hook. See StepCacheable for what each exclusion protects.
//
// OutputSubdir alongside Label because only the first is reliable: an across:
// cell whose name is itself templated keeps an empty Label (see
// config.nameCell, which leaves the naming to the template), while every cell
// is stamped with its coordinates whether or not the matrix collects. Two
// cells that differ ONLY in coordinates would otherwise compute one action key
// between them and reuse each other's outputs.
func stepRecordsOnlyOutputs(step config.Step) bool {
	return !step.Volatile &&
		!step.Routes() &&
		!step.ReadsFrom() &&
		len(step.Verdicts) == 0 &&
		step.Hooks.Empty() &&
		step.Label == "" &&
		step.OutputSubdir == ""
}

// taskCellProducesOutputs reports whether a task cell captures artifacts,
// through its own declaration or the tasks: entry it references. Conservative
// on a resolution error: an unresolvable cell must not be reported cacheable.
func taskCellProducesOutputs(cfg *config.Config, cell config.Step) bool {
	rt, err := cfg.ResolveTask(cell)
	if err != nil {
		return true
	}

	return len(rt.Outputs) > 0
}

// containerUnskippable reports whether a block keeps its chain from being
// cached. Its branches run inside it rather than as chain nodes, but the block's
// hash folds in every branch's content, so a cached success of the block is a
// success of exactly those branches — as long as each one would itself be
// cacheable. A get inside a block is not: the version it fetches is not part of
// the block's content. A try: is never cacheable (a tolerated failure is not a
// success to replay), and a guard or route on the block is a run-time outcome.
func containerUnskippable(cfg *config.Config, step config.Step, kind config.StepKind) (bool, error) {
	if kind == config.StepKindTry || step.When != nil || step.Routes() {
		return true, nil
	}

	for _, branch := range ContainerBranches(step) {
		if branch.Get != "" {
			return true, nil
		}

		_, unskippable, err := planNonGetNode(cfg, branch, 0, "")
		if err != nil || unskippable {
			return true, err
		}
	}

	return false, nil
}

// ContainerBranches is the steps a do:, in_parallel:, race: or ensemble: block
// runs; nil for anything else.
func ContainerBranches(step config.Step) []config.Step {
	//kindswitch:ignore only the blocks with branches answer; every other kind, try: included, has none
	switch {
	case len(step.Do) > 0:
		return step.Do
	case step.InParallel != nil:
		return step.InParallel.Steps
	case step.Race != nil:
		return step.Race.Steps
	case step.Ensemble != nil:
		return step.Ensemble.Agents
	default:
		return nil
	}
}
