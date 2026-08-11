package config

// The verdicts: list — an agent's outcome vocabulary AND where each outcome
// sends the plan, in one ordered declaration.

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// VerdictRoute is one entry of a verdicts: list: the outcome name the model may
// emit, and where the plan goes when it does.
//
// Two spellings, because most verdicts do not branch:
//
//	verdicts:
//	  - approve            # Target "": record the verdict, fall through
//	  - revise: fix-code   # Target "fix-code": record it and jump
//	  - failure: cleanup   # reserved: the step errored or emitted no verdict
//
// The vocabulary and the routing were two fields (verdicts: and to:) that had
// to agree, cross-checked at load. Folding them into one list removes the
// disagreement rather than diagnosing it — and, unlike a to: map, a list keeps
// its order, which verdict mode needs twice over: the order is the emitted tool
// enum, and it is the precedence an ensemble's decide: any resolves by.
//
// The json tags are load-bearing: internal/merkle hashes this list as part of a
// step's identity, since it changes the synthesized tool set.
type VerdictRoute struct {
	Name   string `json:"name"`
	Target string `json:"target,omitempty"`
}

// verdictFailureKey is the one reserved name a verdicts: list may carry: the
// catch for "the step errored or never emitted a verdict". It is excluded from
// the tool enum — the model cannot choose it, the runtime arrives at it — and
// it must route somewhere, since a bare entry would mean "tolerate this
// failure and carry on", which is try:'s job and should not have a second
// spelling here.
const verdictFailureKey = "failure"

// UnmarshalYAML accepts either spelling of an entry: a bare scalar (fall
// through) or a single-key mapping (route). Anything else — a multi-key
// mapping, a sequence — is a load error naming both forms, since a two-key
// entry is an author writing what they think is a map of all their verdicts.
func (v *VerdictRoute) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var name string

		err := node.Decode(&name)
		if err != nil {
			return fmt.Errorf("verdicts entry at line %d: %w", node.Line, err)
		}

		v.Name, v.Target = name, ""

		return nil
	}

	if node.Kind != yaml.MappingNode || len(node.Content) != 2 {
		return fmt.Errorf("verdicts entry at line %d must be either a name on its own (record the verdict and carry on) or a single `name: target` pair (record it and route)", node.Line)
	}

	var name, target string

	err := node.Content[0].Decode(&name)
	if err != nil {
		return fmt.Errorf("verdicts entry at line %d: %w", node.Line, err)
	}

	err = node.Content[1].Decode(&target)
	if err != nil {
		return fmt.Errorf("verdicts entry %q at line %d: target must be a step name or %q: %w", name, node.Line, RouteTargetNext, err)
	}

	v.Name, v.Target = name, target

	return nil
}

// VerdictRoutes returns the verdict list that governs this step: its own, the
// one a try: wraps, or an ensemble block's.
//
// The three live in different places for reasons that survive this redesign —
// a try: wrapper carries no agent fields, and an ensemble's vocabulary belongs
// to the block because every member votes in it — so every caller that asks
// "what does this step route on" goes through here rather than reaching for
// the field.
func (s Step) VerdictRoutes() []VerdictRoute {
	inner := s.Unwrap()

	if inner.Ensemble != nil {
		return inner.Ensemble.Verdicts
	}

	return inner.Verdicts
}

// VerdictNames returns the vocabulary the model may emit, in declaration
// order, excluding the reserved failure catch — which is exactly the enum
// internal/agent puts on the synthesized verdict tool.
func (s Step) VerdictNames() []string {
	routes := s.VerdictRoutes()
	names := make([]string, 0, len(routes))

	for _, route := range routes {
		if route.Name == verdictFailureKey {
			continue
		}

		names = append(names, route.Name)
	}

	return names
}
