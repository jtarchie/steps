package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestResumeContinuesTheJobBudget pins that a job budget is a ceiling on the
// RUN, not on one attempt of it.
//
// The accumulator used to be built fresh on every invocation, so resuming a
// run restarted its allowance at zero: a job stopped at 7M of an 8M budget
// resumed with a full 8M available, and every resume bought another one. The
// runs most likely to be resumed are the expensive ones, which is where that
// costs most.
//
// The fixture: an allowance of 700 tokens and cells spending 400 apiece. The
// first attempt spends 400 and dies on a failing task. The resume must see
// that 400 already gone and refuse to spend another 400 on top — 800 would be
// over the ceiling — so the second agent step never runs.
func TestResumeContinuesTheJobBudget(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()
	fake := newRepeatingFakeLLM(t, says("reviewed").spending(400))

	pipeline := func(failFirst bool) string {
		gate := "true"
		if failFirst {
			gate = "false"
		}

		return fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: reviewer
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }

jobs:
- name: fan
  budget:
    tokens: 700
  plan:
  - agent: reviewer
    inputs: []
    prompt: first pass
  - task: gate
    inputs: []
    run: %[2]s
  - agent: reviewer
    inputs: []
    prompt: second pass
`, fake.URL+"/v1/", gate)
	}

	// Attempt 1: the first agent spends 400, then the gate fails.
	path := writePipeline(t, dir, pipeline(true))

	err := run([]string{"run", path, "--job", "fan"})
	if err == nil {
		t.Fatal("the run succeeded; the fixture needs it to fail at the gate")
	}

	if got := fake.requestCount(); got != 1 {
		t.Fatalf("first attempt made %d provider requests, want 1", got)
	}

	runID := resumeIDFrom(t, path)
	if runID == "" {
		t.Fatal("no run was recorded, so nothing could be resumed")
	}

	// Attempt 2: the gate passes, so the second agent step is reached. It must
	// be refused — 400 is already spent against a 700 ceiling.
	path = writePipeline(t, dir, pipeline(false))

	err = run([]string{"run", path, "--job", "fan", "--resume", runID})
	if err == nil {
		t.Fatal("the resumed run succeeded; the job budget must stop the second agent step")
	}

	// The decisive signal is that the run FAILS at all. A ceiling is checked
	// against the spend a response reports, so the second step's first
	// request still goes out — what changes is that 400 + 400 is now over the
	// 700 ceiling. Had the budget restarted at zero, the step would have had a
	// full 700 to itself, come in at 400, and the run would have SUCCEEDED.
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("resume failed with %v, want a budget breach", err)
	}
}
