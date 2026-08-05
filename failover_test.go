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
