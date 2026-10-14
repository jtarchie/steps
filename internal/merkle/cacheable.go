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

// UncacheableReason names why a step keeps its chain from being skipped, or ""
// when it doesn't. It is the one rule: the planner marks a chain Unskippable
// exactly when this names a reason for one of its steps, and the run's note
// and `steps plan` print the reason itself — two copies of it once drifted.
//
//   - put: and agent: are side effects and non-determinism; a try: tolerates
//     a failure, which is not a success to replay.
//   - when:, to: and context: from: are outcomes only the run knows.
//   - load_var:, approval: and across: produce what they mean at run time.
//   - A task's fix: — including one inherited from its tasks: entry — makes
//     its success depend on what a model did.
//   - A block is as cacheable as its branches: its hash folds in every
//     branch's content, so a cached success of the block is a success of
//     exactly those branches. A get inside one is not, because the version it
//     fetches is not part of the block's content.
func UncacheableReason(cfg *config.Config, step config.Step) (string, error) {
	reason := unskippableReason(step)
	if reason != "" {
		return reason, nil
	}

	if step.Task != "" {
		rt, err := cfg.ResolveTask(step)
		if err != nil {
			return "", fmt.Errorf("resolve task: %w", err)
		}

		if rt.Fix != nil {
			return "fix: agent", nil
		}

		return "", nil
	}

	kind, _ := step.Kind()

	for _, branch := range ContainerBranches(step) {
		if branch.Get != "" {
			return fmt.Sprintf("%s block: get %s inside it", kind, branch.Get), nil
		}

		inner, err := UncacheableReason(cfg, branch)
		if err != nil || inner != "" {
			return fmt.Sprintf("%s block: %s", kind, inner), err
		}
	}

	return "", nil
}

// unskippableReason names the kinds of step that are uncacheable whatever they
// hold — see UncacheableReason for the rest.
func unskippableReason(step config.Step) string {
	switch kind, _ := step.Kind(); kind {
	case config.StepKindPut:
		return "put step"
	case config.StepKindAgent:
		return "agent step"
	case config.StepKindTry:
		return "try step"
	case config.StepKindGet, config.StepKindTask, config.StepKindInParallel, config.StepKindRace,
		config.StepKindEnsemble, config.StepKindLoadVar, config.StepKindApproval, config.StepKindDo:
		// Uncacheable only for what they carry, checked below.
	}

	switch {
	case step.When != nil:
		return "when: guard"
	case step.Routes():
		return "to: routing"
	case step.ReadsFrom():
		return "context: from"
	default:
		return runTimeReason(step)
	}
}

// runTimeReason names the steps whose result does not exist until the run
// produces it: nothing the planner can hash stands in for them.
func runTimeReason(step config.Step) string {
	switch kind, _ := step.Kind(); kind {
	case config.StepKindLoadVar:
		return "load_var step"
	case config.StepKindApproval:
		return "approval step"
	case config.StepKindGet, config.StepKindTask, config.StepKindPut, config.StepKindAgent,
		config.StepKindTry, config.StepKindInParallel, config.StepKindRace, config.StepKindEnsemble, config.StepKindDo:
		// Known at plan time, unless across: below fans them out.
	}

	if len(step.Across) > 0 {
		return "across block"
	}

	return ""
}

// ContainerBranches is the steps a do:, in_parallel:, race: or ensemble: block
// runs; nil for anything else.
func ContainerBranches(step config.Step) []config.Step {
	switch kind, _ := step.Kind(); kind {
	case config.StepKindDo:
		if len(step.Do) > 0 {
			return step.Do
		}
	case config.StepKindInParallel:
		return step.InParallel.Steps
	case config.StepKindRace:
		return step.Race.Steps
	case config.StepKindEnsemble:
		return step.Ensemble.Agents
	case config.StepKindGet, config.StepKindTask, config.StepKindPut, config.StepKindAgent,
		config.StepKindTry, config.StepKindLoadVar, config.StepKindApproval:
		// Only the blocks with branches answer; every other kind, try: included, has none.
	}

	return nil
}
