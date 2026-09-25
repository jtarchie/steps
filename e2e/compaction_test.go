package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// The first reply (~400 estimated tokens) fills the recent window alone, so the opening message is summarizable while the whole conversation still estimates under the 1,000 budget.
func compactionPipeline(t *testing.T, fake *fakeLLM) string {
	t.Helper()

	return writePipeline(t, t.TempDir(), fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: coder
  source: { model: openai/test-model, endpoint: %s, api_key_env: STEPS_TEST_AGENT_API_KEY }
  compact_after_tokens: 1000

jobs:
- name: build
  plan:
  - agent: coder
    messages:
      - "Start the work."
      - "Now finish it."
`, fake.URL+"/v1/"))
}

// The provider reports 5,000 tokens (system prompt, tool schemas, a real tokenizer), which crosses the budget the len/4 estimate never would.
func TestCompactionCountsTheProviderReportedSize(t *testing.T) {
	fake := newFakeLLM(t,
		says(strings.Repeat("progress ", 180)).spending(5000),
		says("Summary of the work so far."),
		says("Done."),
	)

	mustRun(t, compactionPipeline(t, fake))

	if got := fake.requestCount(); got != 3 {
		t.Fatalf("provider requests = %d, want 3 (answer, summarize, answer)", got)
	}

	if !strings.Contains(fake.request(2).systemMessage(), "summarizing a conversation") {
		t.Errorf("request 2 is not the summarization request; system = %q", fake.request(2).systemMessage())
	}
}

// With no usage reported the estimate is all there is, and it is under budget.
func TestCompactionFallsBackToTheEstimateWithoutUsage(t *testing.T) {
	fake := newFakeLLM(t,
		says(strings.Repeat("progress ", 180)),
		says("Done."),
	)

	mustRun(t, compactionPipeline(t, fake))

	if got := fake.requestCount(); got != 2 {
		t.Fatalf("provider requests = %d, want 2 (no compaction without reported usage)", got)
	}
}
