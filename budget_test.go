package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
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
