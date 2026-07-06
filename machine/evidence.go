package machine

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// evidence: — prompt plumbing as data. Measured across the examples, ~38% of
// every machine file is prompt text, and a steady share of THAT is mechanical:
// re-injecting upstream values under a labeled header (ARTICLE:\n${article}),
// and hand-coding the ${critique ? "feedback..." : ""} conditional. evidence:
// declares those blocks; the instruction stays a hand-written prompt:. The
// lowering composes the final prompt as a Dyn.Native (the distill-prompt
// precedent) so the engine renders it through the same Prompt.String path,
// while the instruction and each block function keep first-class dry-run and
// contract checks.

// lowerEvidence replaces the agent's Prompt with the composed
// instruction-plus-blocks value. Called at parse time, after the agent spec
// is built.
func lowerEvidence(a *AgentSpec) {
	instr := a.Prompt
	entries := a.Evidence
	a.instruction = instr

	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		keys = append(keys, e.Key)
	}
	a.Prompt = Dyn{
		Src:    fmt.Sprintf("%s + evidence(%s)", firstLine(instr.Display()), strings.Join(keys, ", ")),
		Native: composeEvidence(instr, entries),
	}
}

// composeEvidence renders: instruction, then each block in declaration order
// as LABEL:\nvalue separated by blank lines. Falsy values (nil/false/"")
// omit their block — conditional feedback with no ternary.
func composeEvidence(instr Dyn, entries []EvidenceEntry) func(scope map[string]any) (any, error) {
	return func(scope map[string]any) (any, error) {
		var parts []string
		s, err := instr.String(scope)
		if err != nil {
			return nil, err
		}
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
		for _, e := range entries {
			var v any
			if e.Value.IsZero() {
				v = scope[e.Key]
			} else {
				v, err = e.Value.Value(scope)
				if err != nil {
					return nil, fmt.Errorf("evidence %s: %w", e.Key, err)
				}
			}
			rendered, err := renderEvidenceValue(e.Key, v)
			if err != nil {
				return nil, err
			}
			if rendered == "" {
				continue
			}
			parts = append(parts, evidenceLabel(e.Key)+":\n"+rendered)
		}
		return strings.Join(parts, "\n\n"), nil
	}
}

// renderEvidenceValue turns a block's value into text: strings pass through,
// structured values render as YAML (the same choice as the yaml() helper),
// falsy values render empty (block omitted).
func renderEvidenceValue(key string, v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case bool:
		if !t {
			return "", nil
		}
		return "", fmt.Errorf("evidence %s: a function returned literal true — return the value to show, or use `%s: true` to inject the scope key", key, key)
	case string:
		return strings.TrimSpace(t), nil
	case map[string]any, []any:
		raw, err := yaml.Marshal(t)
		if err != nil {
			return "", fmt.Errorf("evidence %s: %w", key, err)
		}
		return strings.TrimRight(string(raw), "\n"), nil
	default:
		return fmt.Sprintf("%v", t), nil
	}
}

// evidenceLabel renders a block header from its key: underscores become
// spaces, uppercased — reviewer_feedback -> REVIEWER FEEDBACK.
func evidenceLabel(key string) string {
	return strings.ToUpper(strings.ReplaceAll(key, "_", " "))
}
