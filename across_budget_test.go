package main

// budget: on an across: block — the one ceiling that DEGRADES instead of
// failing. When a matrix has spent its allowance, it stops starting cells,
// the finished ones keep their work, and the plan carries on.

import (
	"fmt"
	"strings"
	"testing"
)

// TestAcrossBudgetStopsAdmittingCells is the whole feature: four cells are
// planned, the allowance covers two, and the plan still reaches the step after
// the matrix.
//
// A job budget would have failed the run here and published nothing. That
// difference is the reason this ceiling exists separately from that one.
func TestAcrossBudgetStopsAdmittingCells(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()

	// Every reply reports 400 tokens, so a 700-token allowance covers exactly
	// two cells: the first is free (nothing spent yet), the second starts at
	// 400, and the third would start at 800 — over.
	fake := newRepeatingFakeLLM(t, says("reviewed").spending(400))

	path := writePipeline(t, dir, fmt.Sprintf(`
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
      values: [a, b, c, d]
    budget:
      tokens: 700
    agent: reviewer
    inputs: []
    messages:
      - "\"Review {{ .vars.item }}\""
  - task: after
    inputs: []
    run: echo "the plan continued"
`, fake.URL+"/v1/"))

	mustRun(t, path)

	nodes := storeNodes(t, path)

	// Two cells ran; the other two were never started, so they recorded
	// nothing at all.
	ran := 0

	for _, node := range nodes {
		if node.Kind == "agent" {
			ran++
		}
	}

	if ran != 2 {
		t.Errorf("agent cells recorded = %d, want 2 (the allowance covers two)", ran)
	}

	// The plan is not failed by the truncation — that is the entire point.
	assertSucceeded(t, nodes, "task", "after")
}

// TestAcrossBudgetLetsARerunFinishTheWork is the property that makes stopping
// tolerable rather than lossy: the cells that ran are cached and the ones that
// never started recorded nothing, so a rerun with a larger allowance picks up
// exactly where the first stopped instead of paying for the whole matrix again.
func TestAcrossBudgetLetsARerunFinishTheWork(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()
	fake := newRepeatingFakeLLM(t, says("reviewed").spending(400))

	pipeline := func(tokens int) string {
		return fmt.Sprintf(`
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
      values: [a, b, c, d]
    budget:
      tokens: %[2]d
    agent: reviewer
    inputs: []
    messages:
      - "\"Review {{ .vars.item }}\""
`, fake.URL+"/v1/", tokens)
	}

	path := writePipeline(t, dir, pipeline(700))
	mustRun(t, path)

	firstRun := fake.requestCount()
	if firstRun != 2 {
		t.Fatalf("provider requests = %d, want 2 on the truncated run", firstRun)
	}

	// A larger allowance. The two finished cells are cache hits — agent cells
	// are unskippable, so they DO re-run; what must not happen is the matrix
	// stopping at two again.
	path = writePipeline(t, dir, pipeline(5000))
	mustRun(t, path)

	if got := fake.requestCount() - firstRun; got != 4 {
		t.Errorf("provider requests on the rerun = %d, want 4 (the whole matrix now fits)", got)
	}
}

// TestAcrossBudgetRejectsWhatItCannotEnforce covers the two spellings that
// would look like a ceiling and be none.
func TestAcrossBudgetRejectsWhatItCannotEnforce(t *testing.T) {
	cases := []struct {
		name, step, extraAgent, wantErr string
	}{
		{
			name: "on a step with no across:",
			step: `  - agent: reviewer
    inputs: []
    messages:
      - hi
    budget:
      tokens: 500`,
			wantErr: "only valid on an across: step",
		},
		{
			name: "in dollars",
			step: `  - across:
    - var: item
      values: [a, b]
    agent: reviewer
    inputs: []
    messages:
      - "\"{{ .vars.item }}\""
    budget:
      usd: 1.5`,
			wantErr: "budget.usd is not valid on an across: step",
		},
		{
			// Budget is one type shared by agents, jobs and across: steps, so
			// reserve_per_cell parses in all three positions. Only the block
			// admits anything, so the other two are rejected rather than
			// reading like configuration that binds nothing.
			name: "a reservation on an agent, which admits nothing",
			step: `  - agent: reserver
    inputs: []
    messages: [hi]`,
			extraAgent: `- name: reserver
  source: { model: openai/test-model, api_key_env: STEPS_TEST_AGENT_API_KEY }
  budget:
    tokens: 500
    reserve_per_cell: 100`,
			wantErr: "budget.reserve_per_cell is only valid on an across: step",
		},
		{
			// A reservation only decides anything when a cell is admitted while
			// another is still running and has reported nothing. Serial settles
			// before admitting, so the field would bind nothing.
			name: "a reservation on a serial matrix, where it decides nothing",
			step: `  - across:
    - var: item
      values: [a, b]
    agent: reviewer
    inputs: []
    messages:
      - "\"{{ .vars.item }}\""
    budget:
      tokens: 500
      reserve_per_cell: 100`,
			wantErr: "budget.reserve_per_cell needs max_in_flight: above 1",
		},
		{
			name: "a negative reservation",
			step: `  - across:
    - var: item
      values: [a, b]
    agent: reviewer
    inputs: []
    messages:
      - "\"{{ .vars.item }}\""
    max_in_flight: 2
    budget:
      tokens: 500
      reserve_per_cell: -1`,
			wantErr: "budget.reserve_per_cell must be a positive number of tokens",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writePipeline(t, dir, `
agents:
- name: reviewer
  source: { model: openai/test-model, api_key_env: STEPS_TEST_AGENT_API_KEY }
`+tc.extraAgent+`

jobs:
- name: fan
  plan:
`+tc.step+"\n")

			err := run([]string{"validate", path})
			if err == nil {
				t.Fatal("the pipeline loaded")
			}

			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestAcrossBudgetBindsUnderConcurrency is the regression for what the review
// pipeline found reviewing this feature: the ceiling was checked BEFORE the
// concurrency slot was acquired, so every admission until the limit saturated
// read a spend of ~0 — the cells it was deciding against were still running.
// A matrix could therefore launch max_in_flight cells before the budget meant
// anything.
//
// Six cells at 400 tokens each against a 700-token allowance, two at a time.
// The first two start blind, which is unavoidable: no spend exists yet. What
// must not happen is all six running.
func TestAcrossBudgetBindsUnderConcurrency(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()
	fake := newRepeatingFakeLLM(t, says("reviewed").spending(400))

	path := writePipeline(t, dir, fmt.Sprintf(`
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
    max_in_flight: 2
    budget:
      tokens: 700
    agent: reviewer
    inputs: []
    messages:
      - "\"Review {{ .vars.item }}\""
`, fake.URL+"/v1/"))

	mustRun(t, path)

	if got := fake.requestCount(); got != 2 {
		t.Errorf("cells run = %d, want exactly 2 (a 700 allowance covers two 400-token cells)", got)
	}
}

// TestAcrossBudgetBindsAtFullWidth is the case a spent()-only ceiling could
// never bind, and the reason budget.reserve_per_cell: exists.
//
// max_in_flight covers every cell, so newLimiter hands back a limiter that
// never blocks and no cell finishes before the last one is admitted. Without a
// reservation every admission reads a total of ~0 and all six cells start —
// the block's ceiling bounding precisely nothing. Reserving 400 per cell means
// the 700 allowance is consumed after two, and the third is refused.
func TestAcrossBudgetBindsAtFullWidth(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()
	fake := newRepeatingFakeLLM(t, says("reviewed").spending(400))

	path := writePipeline(t, dir, fmt.Sprintf(`
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
      tokens: 700
      reserve_per_cell: 400
    agent: reviewer
    inputs: []
    messages:
      - "\"Review {{ .vars.item }}\""
`, fake.URL+"/v1/"))

	mustRun(t, path)

	// Exactly two: 0 + 400 reserved admits the first, 400 + 400 >= 700 refuses
	// the third, and the finished cells' real spend (400 apiece) keeps it
	// refused. An inequality here would pass at one cell and at zero, which is
	// how an over-truncating budget hid.
	if got := fake.requestCount(); got != 2 {
		t.Errorf("cells run = %d, want exactly 2 (a 700 allowance covers two 400-token cells)", got)
	}
}

// TestAcrossBudgetReservesTheCellAgentsOwnBudget proves the reservation does
// not require new config: an agent that already declares what one invocation
// may cost supplies the number, so a block inherits a binding ceiling without
// naming reserve_per_cell: at all.
func TestAcrossBudgetReservesTheCellAgentsOwnBudget(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()
	fake := newRepeatingFakeLLM(t, says("reviewed").spending(400))

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

defaults:
  preflight:
    disabled: true

agents:
- name: reviewer
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
  budget:
    tokens: 400

jobs:
- name: fan
  plan:
  - across:
    - var: item
      values: [a, b, c, d, e, f]
    max_in_flight: 6
    budget:
      tokens: 700
    agent: reviewer
    inputs: []
    messages:
      - "\"Review {{ .vars.item }}\""
`, fake.URL+"/v1/"))

	mustRun(t, path)

	if got := fake.requestCount(); got != 2 {
		t.Errorf("cells run = %d, want exactly 2: the cell agent's own budget.tokens (400) is the reservation", got)
	}
}
