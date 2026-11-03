package merkle

import (
	"errors"
	"fmt"

	"github.com/jtarchie/steps/internal/config"
)

// withWhen folds a step's when: guard command into content, but only when the
// step carries one — so a step without a guard hashes byte-identically to
// before this field existed (the same value-gating as image:). The guard
// decides whether the step executes at all, so changing it must invalidate the
// cache. Only the command is hashed: its *outcome* is a run-time fact the
// planner cannot know, and a cached node was by definition produced by a run
// the guard already allowed.
func withWhen(step config.Step, content map[string]any) map[string]any {
	if step.When != nil && step.When.Run != "" {
		content["when"] = step.When.Run
	}

	return content
}

// withRouting folds a step's routing surface — to:, max_visits:, and (for a
// verdict agent) verdicts: — into content, but only when set, so a step with
// no routing hashes byte-identically to before these fields existed (the same
// value-gating as image:/when:). The chain containing a routing step is already
// unconditionally unskippable (see internal/pipeline's chainUnskippable and
// planNonGetNode below), so this isn't needed for skip-decision correctness —
// it's so a node hash shared across chains (e.g. via get: version: every fan-
// out) still changes when the routing changes, instead of colliding with a
// stale cached success from before the edit. verdicts: matters because it
// changes the synthesized required verdict tool set (internal/agent), so it
// genuinely alters what the step executes.
func withRouting(step config.Step, content map[string]any) map[string]any {
	if step.To != nil {
		content["to"] = step.To // map[string]string — json.Marshal sorts keys, so the hash stays deterministic
		content["max_visits"] = step.MaxVisits
	}

	if len(step.Verdicts) != 0 {
		// Order-significant: it is the emitted enum. A []VerdictRoute marshals
		// as an array of {name, target} objects, so the order survives into the
		// hash the way a map's keys could not — which is the whole reason the
		// targets live in this list rather than in a to: map beside it.
		content["verdicts"] = step.Verdicts
	}

	// A computed Label is the name the step is known by (see config.Step.Label),
	// and it belongs to identity for a concrete reason: a matrix whose author
	// interpolated nothing produces cells that differ ONLY by their
	// coordinates. Those coordinates used to sit in the task: field and entered
	// the hash through it; now they sit here. Leave them out and every such
	// cell collapses onto one hash — the matrix runs once and reports itself
	// cached for the rest.
	//
	// Folded in withRouting rather than at a single call site because the
	// runners hash through the leaf builders directly (TaskNodeContent,
	// AgentContentMap, PutNodeContent) while cache lookups go through
	// stepContentMap. Both paths pass through here, and a label present on one
	// but not the other is a cell that can never match its own recorded node.
	if step.Label != "" {
		content["label"] = step.Label
	}

	// What a step READS from other steps is part of what it is. It matters
	// most for a TASK, which is skippable: a cached command never runs, so a
	// task whose from: changed would otherwise replay an outcome produced
	// without the decision it now asks for. (An agent step is unconditionally
	// unskippable already; this keeps both kinds honest by one rule.)
	if senders := step.FromSenders(); len(senders) > 0 {
		from := step.ContextFrom()
		levels := make([]string, 0, len(senders))

		for _, sender := range senders {
			levels = append(levels, sender+"="+string(from[sender]))
		}

		content["context_from"] = levels
	}

	return content
}

// withHooks folds a step's resolved hook content into content, but only when
// the step actually carries hooks — so a step with no hooks hashes
// byte-identically to before this field existed (the same value-gating as
// image:). Each hook's content is its own resolved content map, so editing a
// hook, or the tasks:/agents: entry it references, invalidates the enclosing
// step's hash. Nested hooks recurse through the same builders.
func withHooks(cfg *config.Config, step config.Step, content map[string]any) (map[string]any, error) {
	if step.Hooks.Empty() {
		return content, nil
	}

	hooks, err := hooksContent(cfg, step.Hooks)
	if err != nil {
		return nil, err
	}

	content["hooks"] = hooks

	return content, nil
}

// withStepModifiers folds what every step kind shares — routing, when: and
// hooks — into content.
func withStepModifiers(cfg *config.Config, step config.Step, content map[string]any) (map[string]any, error) {
	return withHooks(cfg, step, withWhen(step, withRouting(step, content)))
}

func hooksContent(cfg *config.Config, hooks config.Hooks) (map[string]any, error) {
	out := map[string]any{}

	err := hooks.Each(func(name string, step *config.Step) error {
		hc, err := hookContentMap(cfg, *step)
		if err != nil {
			return fmt.Errorf("%s hook: %w", name, err)
		}

		out[name] = hc

		return nil
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // already wrapped with the hook name above
	}

	return out, nil
}

// hookContentMap builds the hashed content for one hook step through the same
// builders a plan step uses (which recurse into the hook's own hooks). Only
// task/put/agent/try are hook bodies; LoadConfig rejects the rest, and this
// refuses them rather than hashing something no run executes.
func hookContentMap(cfg *config.Config, step config.Step) (map[string]any, error) {
	kind, _ := step.Kind()

	switch kind { //nolint:exhaustive // default covers every kind that is not a hook body
	case config.StepKindTask, config.StepKindPut, config.StepKindAgent, config.StepKindTry:
		return stepContentMap(cfg, step)
	default:
		return nil, errors.New("unrecognized hook step (must be task, put, agent, or try)")
	}
}
