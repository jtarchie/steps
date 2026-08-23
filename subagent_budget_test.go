package main

import (
	"fmt"
	"testing"
)

// TestSubAgentRunsWithoutAnyBudget is the end-to-end regression for the worst
// shape the delegation-budget work could have shipped in.
//
// A sub-agent's allowance is a share of what its parent has LEFT, but 0 means
// UNBOUNDED, not empty. Reading an unbudgeted parent's "no ceiling" as "no
// allowance left" refused every delegation on every pipeline that declares no
// budgets — which is most of them — and told the model its parent's budget was
// spent when there was no budget at all.
//
// It passed the doc-scenario suite because a scripted fake is not asserted to
// be exhausted: with the child skipped, the parent simply consumed the reply
// scripted for it and the step still succeeded. So this asserts the child
// actually ran, by its own request landing on the wire.
func TestSubAgentRunsWithoutAnyBudget(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()

	// Routed rather than positional: the parent and the child each get the
	// reply meant for them, so a skipped child cannot silently consume the
	// other's turn.
	fake := newRoutedFakeLLM(t, func(req capturedRequest) turn {
		if req.systemMessage() == "You summarize." {
			return says("summarized")
		}

		for _, msg := range req.Messages {
			for _, call := range msg.ToolCalls {
				if call.Function.Name == "summarizer" {
					return says("done, the helper answered")
				}
			}
		}

		return callsTool("summarizer", map[string]any{"request": "summarize it"})
	})

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: summarizer
  system: "You summarize."
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }

- name: lead
  system: "You lead."
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools:
  - agent: summarizer
    description: Summarizes things.

jobs:
- name: work
  plan:
  - agent: lead
    inputs: []
    messages:
      - Delegate the summary.
    assert:
      tool_calls:
      - name: summarizer
`, fake.URL+"/v1/"))

	mustRun(t, "run", path, "--job", "work")

	// The child's own conversation reached the provider. Two requests would be
	// the parent alone (delegate, then finish); the child's makes a third.
	if got := fake.requestCount(); got < 3 {
		t.Errorf("provider requests = %d, want at least 3: the sub-agent never ran on a pipeline with no budgets", got)
	}
}
