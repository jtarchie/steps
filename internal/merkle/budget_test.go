package merkle

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestBudgetIsNotHashed pins the rule that a budget is an operational limit,
// like timeout: and assert:, and must never enter a step's content hash.
//
// It matters more than it looks. The first thing an operator does after seeing
// a usage report is set a ceiling — and if that invalidated the cache, doing
// so would re-run every previously-succeeded step in the pipeline, which is
// the exact expense a budget exists to control.
func TestBudgetIsNotHashed(t *testing.T) {
	t.Parallel()

	step := config.Step{Agent: "reviewer", Prompt: "do it"}

	contentFor := func(t *testing.T, budget *config.Budget) map[string]any {
		t.Helper()

		cfg := &config.Config{Agents: []config.Agent{{
			Name:   "reviewer",
			Source: config.AgentSource{Model: "openai/gpt-4o", APIKeyEnv: "TEST_KEY"},
			Budget: budget,
		}}}

		ri, err := cfg.ResolveAgentInvocation(step)
		if err != nil {
			t.Fatalf("ResolveAgentInvocation: %v", err)
		}

		content, err := AgentContentMap(cfg, step, ri)
		if err != nil {
			t.Fatalf("AgentContentMap: %v", err)
		}

		return content
	}

	without := contentFor(t, nil)
	with := contentFor(t, &config.Budget{Tokens: 200_000})

	if _, present := with["budget"]; present {
		t.Error("budget appears in the hashed content")
	}

	hash := func(content map[string]any) string {
		t.Helper()

		out, err := HashNode(NodeKindAgent, content, "")
		if err != nil {
			t.Fatalf("HashNode: %v", err)
		}

		return out
	}

	if hash(without) != hash(with) {
		t.Errorf("adding a budget changed the step's content hash:\n without: %v\n with:    %v", without, with)
	}
}
