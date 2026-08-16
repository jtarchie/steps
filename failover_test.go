package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/pipeline"
)

// failoverPipeline points an agent at a dead primary with one live fallback.
func failoverPipeline(t *testing.T, dir, deadURL, liveURL string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
agents:
- name: writer
  source:
    endpoint: %[1]s/v1/
    model: primary-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  fallback:
  - source:
      endpoint: %[2]s/v1/
      model: backup-model
      api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: publish
  plan:
  - agent: writer
    inputs: []
    prompt: Write something.
`, deadURL, liveURL))
}

// TestFailoverUsesTheBackupWhenThePrimaryIsDown is the whole story: a model
// that went unavailable upstream killed three consecutive runs over roughly 50
// minutes, and the manual fix was to point the agent at a different model —
// one line. This automates that line.
func TestFailoverUsesTheBackupWhenThePrimaryIsDown(t *testing.T) {
	pipeline.ResetPreflightCache()
	// Preflight pins "writer" (or "has-fallback") to whichever source
	// answered, in a process-global cache that otherwise outlives this test —
	// the next test in the binary to declare an agent of the same name would
	// silently inherit a torn-down fake server's URL. Clearing on the way out
	// too, not just the way in, is what makes that impossible regardless of
	// test execution order.
	t.Cleanup(pipeline.ResetPreflightCache)

	dir := t.TempDir()

	outage := make([]turn, 3)
	for i := range outage {
		outage[i] = failsWith(http.StatusInternalServerError)
	}

	dead := newFakeLLM(t, outage...)
	live := newFakeLLM(t, says("probe ok"), says("done"))

	path := failoverPipeline(t, dir, dead.URL, live.URL)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	logs := captureStderr(t)

	err := run([]string{"run", path, "--job", "publish"})
	stderr := logs()

	if err != nil {
		t.Fatalf("run failed despite a healthy fallback: %v", err)
	}

	// The conversation went to the backup, not the dead primary.
	if got := live.requestCount(); got != 2 {
		t.Errorf("backup requests = %d, want 2 (its probe and the conversation)", got)
	}

	// Visible, not silently absorbed: a fallback model can produce
	// meaningfully different output, and a quality dip caused by an outage
	// must not look identical to a normal run.
	if !strings.Contains(stderr, "agent.failover") {
		t.Errorf("the run did not say it failed over:\n%s", stderr)
	}
}

// TestFailoverFailsWhenEverySourceIsDown verifies a fallback is a fallback,
// not a guarantee: with nothing healthy the run still stops before any step.
func TestFailoverFailsWhenEverySourceIsDown(t *testing.T) {
	pipeline.ResetPreflightCache()
	// Preflight pins "writer" (or "has-fallback") to whichever source
	// answered, in a process-global cache that otherwise outlives this test —
	// the next test in the binary to declare an agent of the same name would
	// silently inherit a torn-down fake server's URL. Clearing on the way out
	// too, not just the way in, is what makes that impossible regardless of
	// test execution order.
	t.Cleanup(pipeline.ResetPreflightCache)

	dir := t.TempDir()

	outage := make([]turn, 3)
	for i := range outage {
		outage[i] = failsWith(http.StatusInternalServerError)
	}

	dead := newFakeLLM(t, outage...)
	alsoDead := newFakeLLM(t, outage...)

	path := failoverPipeline(t, dir, dead.URL, alsoDead.URL)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{"run", path, "--job", "publish"})
	if err == nil {
		t.Fatal("run succeeded with every source down")
	}

	if !strings.Contains(err.Error(), "preflight failed") {
		t.Errorf("error does not report the preflight failure: %v", err)
	}
}

// TestFailoverAppliesToEveryAgentSharingAModel covers a hole that made
// preflight report a clean bill of health for a run that could not start.
//
// Model probes are deduped by (endpoint, model) so a shared model costs one
// request. The failover DECISION was deduped along with it, so a second agent
// on the same dead model was skipped entirely: nothing was recorded for it,
// nothing was reported, and the run died mid-plan against the dead primary
// while the log said preflight passed.
func TestFailoverAppliesToEveryAgentSharingAModel(t *testing.T) {
	pipeline.ResetPreflightCache()
	// Preflight pins "writer" (or "has-fallback") to whichever source
	// answered, in a process-global cache that otherwise outlives this test —
	// the next test in the binary to declare an agent of the same name would
	// silently inherit a torn-down fake server's URL. Clearing on the way out
	// too, not just the way in, is what makes that impossible regardless of
	// test execution order.
	t.Cleanup(pipeline.ResetPreflightCache)

	dir := t.TempDir()

	outage := make([]turn, 4)
	for i := range outage {
		outage[i] = failsWith(http.StatusInternalServerError)
	}

	dead := newFakeLLM(t, outage...)
	live := newFakeLLM(t, says("probe ok"), says("probe ok"), says("done"), says("done"))

	// Both agents run the same model on the same dead endpoint. Only
	// `has-fallback` declares an alternate.
	path := writePipeline(t, dir, fmt.Sprintf(`
agents:
- name: has-fallback
  source:
    endpoint: %[1]s/v1/
    model: shared-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  fallback:
  - source:
      endpoint: %[2]s/v1/
      model: backup-model
      api_key_env: STEPS_TEST_AGENT_API_KEY
- name: no-fallback
  source:
    endpoint: %[1]s/v1/
    model: shared-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: publish
  plan:
  - agent: has-fallback
    inputs: []
    prompt: Plan it.
  - agent: no-fallback
    inputs: []
    prompt: Build it.
`, dead.URL, live.URL))

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{"run", path, "--job", "publish"})
	if err == nil {
		t.Fatal("preflight passed with an agent whose only model is down and has no fallback")
	}

	if !strings.Contains(err.Error(), "no-fallback") {
		t.Errorf("preflight did not name the agent that cannot run: %v", err)
	}
}
