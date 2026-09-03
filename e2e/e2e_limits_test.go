package e2e

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// This file covers the limit-dial convention: an omitted dial takes the
// package default, an explicit 0 means NO limit, and a dial set on the
// agents: entry is inherited by every step that names it.
//
// Both halves are end-to-end because both are cross-layer: the value is
// written in YAML, resolved in internal/config, and only observable in what
// internal/agent does with it — a package-level test of either end would
// assert on the seam rather than the behavior.

// TestAgentMaxTurnsZeroIsUnlimited proves `max_turns: 0` removes the cap
// rather than falling back to the default 30.
//
// The discriminator is the request count, not the outcome: a conversation
// that exhausts its turns still SUCCEEDS (the runner takes the tools away
// and asks for an answer from what it has — see
// TestAgentAnswersWhenTurnsRunOut), so the only way to tell "ran past 30"
// from "was cut off at 30 and wrapped up" is to count what the provider was
// asked. Capped at the default this fake would be called 31 times (30 tool
// turns plus the tool-less wrap-up); uncapped it is called 36 (35 tool turns
// plus the model's own final answer).
func TestAgentMaxTurnsZeroIsUnlimited(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()

	const toolTurns = 35

	var seen atomic.Int64

	// Each turn searches for a DIFFERENT string. Repeating one identical
	// interaction would trip the loop detector at five copies (see
	// internal/agent/loop.go) and fail the attempt long before turn 30,
	// which would prove nothing about the cap.
	fake := newRoutedFakeLLM(t, func(_ capturedRequest) turn {
		n := seen.Add(1)
		if n > toolTurns {
			return says("done after every turn I wanted")
		}

		return callsTool("search_files", map[string]any{"query": fmt.Sprintf("needle-%d", n)})
	})

	path := writePipeline(t, dir, `
defaults:
  preflight:
    disabled: true

agents:
- name: tireless
  max_turns: 0
  source: { model: openai/test-model, endpoint: `+fake.URL+`/v1/, api_key_env: STEPS_TEST_AGENT_API_KEY }

jobs:
- name: look
  plan:
  - agent: tireless
    inputs: []
    messages:
      - Investigate the repository.
`)

	mustRun(t, path)

	assertSucceeded(t, storeNodes(t, path), "agent", "tireless")

	if got, want := fake.requestCount(), toolTurns+1; got != want {
		t.Errorf("provider requests = %d, want %d — the step was cut off at a cap max_turns: 0 should have removed", got, want)
	}
}

// TestAgentEntryTimeoutIsInheritedByItsSteps proves `timeout:` on an agents:
// entry reaches the steps that name it, and that a step's own value still
// wins.
//
// This is what collapses examples/pr-review.yml's five copies of
// `timeout: 20m` into one line: before it, timeout: was the only dial an
// agent entry could not carry, so a deadline shared by every step of one
// agent had to be repeated on each of them.
//
// 1ns is not a realistic deadline — it is a deterministic one. Any real
// value would make this test a race against an httptest round trip.
func TestAgentEntryTimeoutIsInheritedByItsSteps(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()
	fake := newRoutedFakeLLM(t, func(_ capturedRequest) turn { return says("fine") })

	path := writePipeline(t, dir, `
defaults:
  preflight:
    disabled: true

agents:
- name: hasty
  timeout: 1ns
  source: { model: openai/test-model, endpoint: `+fake.URL+`/v1/, api_key_env: STEPS_TEST_AGENT_API_KEY }

jobs:
- name: expires
  plan:
  - agent: hasty
    inputs: []
    messages:
      - Say something.
`)

	err := cli.Run([]string{"run", path, "--job", "expires"})
	if err == nil {
		t.Fatal("run succeeded, but the agent entry's timeout: 1ns should have expired the step")
	}

	if !strings.Contains(err.Error(), "deadline") {
		t.Errorf("error = %v, want one naming an expired deadline", err)
	}
}

// TestStepTimeoutBeatsAgentEntryTimeout is the other half of the precedence:
// the narrower statement wins, exactly as max_turns: already behaves.
func TestStepTimeoutBeatsAgentEntryTimeout(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()
	fake := newRoutedFakeLLM(t, func(_ capturedRequest) turn { return says("fine") })

	path := writePipeline(t, dir, `
defaults:
  preflight:
    disabled: true

agents:
- name: hasty
  timeout: 1ns
  source: { model: openai/test-model, endpoint: `+fake.URL+`/v1/, api_key_env: STEPS_TEST_AGENT_API_KEY }

jobs:
- name: patient
  plan:
  - agent: hasty
    inputs: []
    timeout: 5m
    messages:
      - Say something.
`)

	mustRun(t, path)

	assertSucceeded(t, storeNodes(t, path), "agent", "hasty")
}

// TestAgentEntryAttemptsIsInheritedByItsSteps proves attempts: on an agents:
// entry reaches its steps.
//
// It asserts on attempts: 1 rather than a larger number because the default
// is already 3: a test that scripted two 503s and a success would pass
// whether or not the entry's value was read at all. One attempt means the
// first 503 is terminal, which only the inherited value produces.
func TestAgentEntryAttemptsIsInheritedByItsSteps(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()
	fake := newFakeLLM(t, failsWith(503), says("recovered"))

	path := writePipeline(t, dir, `
defaults:
  preflight:
    disabled: true

agents:
- name: onceonly
  attempts: 1
  source: { model: openai/test-model, endpoint: `+fake.URL+`/v1/, api_key_env: STEPS_TEST_AGENT_API_KEY }

jobs:
- name: flaky
  plan:
  - agent: onceonly
    inputs: []
    messages:
      - Say something.
`)

	err := cli.Run([]string{"run", path, "--job", "flaky"})
	if err == nil {
		t.Fatal("run succeeded, but attempts: 1 on the agent entry should have made the first 503 terminal")
	}

	if got := fake.requestCount(); got != 1 {
		t.Errorf("provider requests = %d, want 1 — the step retried past the entry's attempts: 1", got)
	}
}
