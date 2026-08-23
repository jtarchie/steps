package main

import (
	"fmt"
	"testing"
)

// acrossWaitPipeline is a full-width matrix: max_in_flight covers every cell,
// so nothing finishes before the last one is admitted and the whole decision
// runs on reservations.
func acrossWaitPipeline(t *testing.T, dir, endpoint string, ceiling, reserve int) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

defaults:
  preflight:
    disabled: true

agents:
- name: reviewer
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }

jobs:
- name: fan
  plan:
  - across:
    - var: item
      values: [a, b, c, d, e, f]
    max_in_flight: 6
    budget:
      tokens: %[2]d
      reserve_per_cell: %[3]d
    agent: reviewer
    inputs: []
    messages:
      - "\"Review {{ .vars.item }}\""
`, endpoint, ceiling, reserve))
}

// TestAcrossBudgetWaitsRatherThanTruncating is the regression for a matrix
// that stopped nowhere near its ceiling.
//
// A refusal caused by SPEND is permanent; one caused by RESERVATIONS is not,
// because in-flight cells release theirs as they finish. Treating the second
// as permanent stopped this matrix after four of six cells having spent forty
// tokens of three thousand six hundred — the reservations it stopped on were
// released milliseconds later.
func TestAcrossBudgetWaitsRatherThanTruncating(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()
	fake := newRepeatingFakeLLM(t, says("reviewed").spending(10))

	mustRun(t, acrossWaitPipeline(t, dir, fake.URL+"/v1/", 3600, 900))

	// Every cell must run: six of them cost 60 tokens against a 3,600 ceiling.
	if got := fake.requestCount(); got != 6 {
		t.Errorf("cells run = %d, want all 6: the ceiling is 3,600 and the whole matrix costs 60", got)
	}
}

// TestAcrossBudgetStillBindsOnRealSpend is the control: waiting must not turn
// the ceiling off. The same full-width matrix with cells that genuinely cost
// more than the allowance still stops.
func TestAcrossBudgetStillBindsOnRealSpend(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()
	// 900 apiece against a 2,000 ceiling: the third cell's admission is the
	// first that can see 1,800 of real spend, and the fourth cannot fit.
	fake := newRepeatingFakeLLM(t, says("reviewed").spending(900))

	mustRun(t, acrossWaitPipeline(t, dir, fake.URL+"/v1/", 2000, 900))

	if got := fake.requestCount(); got >= 6 {
		t.Errorf("cells run = %d; every cell ran despite the allowance covering barely two", got)
	}
}
