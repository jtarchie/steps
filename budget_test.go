package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// budgetPipeline renders a one-agent pipeline with an optional agent budget,
// an optional job budget, and hooks on both paths so a test can prove which
// one fired.
func budgetPipeline(t *testing.T, dir, endpoint, agentBudget, jobBudget string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: writer
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]
%[2]s

jobs:
- name: publish
%[3]s
  plan:
  - agent: writer
    inputs: []
    prompt: Write something.
    on_failure:
      task: on-failure
      inputs: []
      run: echo fired >> %[4]s
    on_error:
      task: on-error
      inputs: []
      run: echo fired >> %[5]s
`, endpoint, agentBudget, jobBudget,
		filepath.Join(dir, "on_failure.log"), filepath.Join(dir, "on_error.log")))
}

// TestAgentBudgetStopsTheStep covers the enforcement half of budgets: an agent
// step that blows its ceiling stops rather than continuing to spend, and the
// breach classifies as errored — an operational limit being hit, not the model
// producing a bad answer — so on_error fires and on_failure does not.
func TestAgentBudgetStopsTheStep(t *testing.T) {
	dir := t.TempDir()

	// Each turn reports 60 tokens against a 100-token cap, so the second one
	// crosses it. The script is longer than the run should get through: if the
	// cap were not enforced the conversation would finish and the job pass.
	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": "true"}).spending(60),
		callsTool("run_shell", map[string]any{"command": "true"}).spending(60),
		says("done").spending(60),
	)

	path := budgetPipeline(t, dir, fake.URL, "  budget:\n    tokens: 100", "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	logs := captureStderr(t)

	err := run([]string{"run", path, "--job", "publish"})
	stderr := logs()

	if err == nil {
		t.Fatal("job succeeded despite blowing its token budget")
	}

	if !strings.Contains(err.Error(), "budget exceeded") {
		t.Errorf("error does not name the budget: %v", err)
	}

	// The step stopped at the breach instead of running the rest of the
	// script — the whole point of a ceiling.
	if got := fake.requestCount(); got != 2 {
		t.Errorf("provider requests = %d, want 2 (the step must stop at the breach)", got)
	}

	assertLineCount(t, filepath.Join(dir, "on_error.log"), 1)
	assertNoFile(t, filepath.Join(dir, "on_failure.log"))

	// Reporting is the half that carries no risk, and it happens either way.
	if !strings.Contains(stderr, "job.usage") {
		t.Errorf("the run reported no usage at all:\n%s", stderr)
	}
}

// TestJobBudgetReportsTheRunningTotal covers the attribution decision: a job
// breach is cumulative, so naming only the step that crossed the line would be
// misleading — the step that trips it is rarely the one that cost the most.
func TestJobBudgetReportsTheRunningTotal(t *testing.T) {
	dir := t.TempDir()

	fake := newFakeLLM(t,
		says("first").spending(80),
		says("second").spending(80),
	)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: planner
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
- name: coder
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: publish
  budget:
    tokens: 100
  plan:
  - agent: planner
    inputs: []
    prompt: Plan it.
  - agent: coder
    inputs: []
    prompt: Build it.
`, fake.URL))

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{"run", path, "--job", "publish"})
	if err == nil {
		t.Fatal("job succeeded despite blowing its job-level token budget")
	}

	message := err.Error()

	if !strings.Contains(message, "job budget exceeded") {
		t.Errorf("error does not name the job budget: %v", err)
	}

	// Both steps named, with the one that tripped it marked — not just the
	// final number.
	for _, want := range []string{"planner 80", "coder 80", "tripped here"} {
		if !strings.Contains(message, want) {
			t.Errorf("error does not report the running total (missing %q): %v", want, err)
		}
	}
}

// TestAgentUsageIsPersisted covers the whole point of recording spend: that it
// is still answerable after the process exits.
//
// Before this, usage was a fmt.Printf and nothing else — so "what did this run
// cost" and "which step is the sink" were answerable only by scrolling
// terminal scrollback, which is how every ceiling in examples/pr-review.yml
// came to be derived by hand.
func TestAgentUsageIsPersisted(t *testing.T) {
	dir := t.TempDir()

	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": "true"}).spending(60),
		says("done").spending(40),
	)

	path := budgetPipeline(t, dir, fake.URL, "", "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{"run", path, "--job", "publish"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	st, err := store.OpenStore(statePath(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()

	totals, err := st.RunCostTotals(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(totals) != 1 {
		t.Fatalf("recorded %d runs of usage, want 1", len(totals))
	}

	if totals[0].Tokens != 100 {
		t.Errorf("recorded %d tokens, want 100 (the provider reported 60 + 40)", totals[0].Tokens)
	}

	// Unpriced, not $0.00: nothing in the request path reports dollars, and a
	// zero would say the run was free rather than that nobody priced it.
	if totals[0].CostUSD != nil {
		t.Errorf("cost = %v, want nil — no provider path reports a dollar figure yet", *totals[0].CostUSD)
	}

	assertRecordedStepUsage(t, st, totals[0].RunID)
}

// assertRecordedStepUsage checks the per-step row behind the rollup: the
// metadata is what makes a token count actionable afterwards.
func assertRecordedStepUsage(t *testing.T, st *store.Store, runID string) {
	t.Helper()

	usage, err := st.RunUsage(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}

	if len(usage) != 1 {
		t.Fatalf("recorded %d agent steps, want 1", len(usage))
	}

	step := usage[0]
	if step.Total != 100 || step.Prompt == 0 {
		t.Errorf("step usage = %+v, want the provider's reported prompt/total", step)
	}

	if step.ModelReq == "" {
		t.Error("the requested model was not recorded, so a run cannot say what it asked for")
	}

	if step.NodeHash == "" {
		t.Error("the node hash was not recorded, so spend cannot be tied back to the step that caused it")
	}

	// The raw block is the future-proofing: the schema has no versioning, so a
	// field not captured now could never be backfilled.
	if step.RawMeta == "" {
		t.Error("the provider's usage block was not kept")
	}
}

// TestAgentUsageRecordsEveryCellOfAMatrix guards the keying of agent_usage.
//
// Every agent step inside a block reports the BLOCK's plan index — six across:
// cells, an ensemble's members and a do:'s children all share one. Keying the
// table on that index kept the last one and silently overwrote the rest, so a
// six-cell review matrix reported one reviewer and under-counted the run by
// the whole fan-out: the exact pipeline this feature exists to make legible.
func TestAgentUsageRecordsEveryCellOfAMatrix(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()

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
    - var: shard
      values: [a, b, c]
    agent: reviewer
    prompt: "review {{ .vars.shard }}"
`, fake.URL))

	err := run([]string{"run", path, "--job", "fan"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	st, err := store.OpenStore(statePath(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()

	totals, err := st.RunCostTotals(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(totals) != 1 {
		t.Fatalf("recorded %d runs, want 1", len(totals))
	}

	// Three cells at 400 each. Under the old keying this was 400: one row,
	// overwritten twice.
	if totals[0].Steps != 3 {
		t.Errorf("recorded %d agent steps, want 3 — the matrix's cells share a plan index and must not overwrite each other", totals[0].Steps)
	}

	if totals[0].Tokens != 1200 {
		t.Errorf("recorded %d tokens, want 1200 (3 cells x 400)", totals[0].Tokens)
	}

	assertCellsNamedByCoordinate(t, st, totals[0].RunID)
}

// assertCellsNamedByCoordinate checks each cell is recorded under its own
// label, which is what makes a cost report say WHICH reviewer was expensive
// rather than only that one was.
func assertCellsNamedByCoordinate(t *testing.T, st *store.Store, runID string) {
	t.Helper()

	usage, err := st.RunUsage(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, step := range usage {
		seen[step.StepName] = true
	}

	for _, want := range []string{"reviewer [shard=a]", "reviewer [shard=b]", "reviewer [shard=c]"} {
		if !seen[want] {
			t.Errorf("no usage row for %q; recorded %v", want, seen)
		}
	}
}
