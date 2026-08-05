package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/pipeline"
)

// preflightPipeline renders a job whose plan runs a task before its agent, so
// a test can prove preflight stopped the run before ANY step — not merely
// before the agent step.
func preflightPipeline(t *testing.T, dir, endpoint, extra string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
agents:
- name: writer
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
%[2]s

jobs:
- name: publish
  plan:
  - task: prepare
    inputs: []
    run: echo ran >> %[3]s
  - agent: writer
    inputs: []
    prompt: Write something.
`, endpoint, extra, filepath.Join(dir, "task.log")))
}

// TestPreflightStopsBeforeAnyStepRuns is the whole point of the feature: a
// model that is not serving is discovered in seconds, before the plan spends
// anything, rather than at the moment the agent step is finally reached — which
// for a real plan was half an hour and a chunk of a capped budget in.
func TestPreflightStopsBeforeAnyStepRuns(t *testing.T) {
	pipeline.ResetPreflightCache()

	dir := t.TempDir()

	outage := make([]turn, 5)
	for i := range outage {
		outage[i] = failsWith(http.StatusInternalServerError)
	}

	fake := newFakeLLM(t, outage...)
	path := preflightPipeline(t, dir, fake.URL, "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{"run", path, "--job", "publish"})
	if err == nil {
		t.Fatal("run succeeded against a model that never answers")
	}

	// The message has to say nothing ran, or a reader cannot tell this from
	// an ordinary mid-plan failure — and "nothing ran" is the entire value.
	for _, want := range []string{"preflight failed", "no steps were run", "test-model"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q: %v", want, err)
		}
	}

	// The task ahead of the agent in the plan never ran either.
	assertNoFile(t, filepath.Join(dir, "task.log"))
}

// TestPreflightPassesThroughToTheRun verifies a live model costs one probe and
// then gets out of the way.
func TestPreflightPassesThroughToTheRun(t *testing.T) {
	pipeline.ResetPreflightCache()

	dir := t.TempDir()
	fake := newFakeLLM(t, says("probe ok"), says("done"))
	path := preflightPipeline(t, dir, fake.URL, "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	assertLineCount(t, filepath.Join(dir, "task.log"), 1)

	if got := fake.requestCount(); got != 2 {
		t.Errorf("provider requests = %d, want 2 (one probe, one conversation turn)", got)
	}
}

// TestPreflightCachesAcrossRuns pins the requirement that makes preflight
// usable under `steps watch`: without a cache, every poll interval pays for a
// probe request against every model in the pipeline.
func TestPreflightCachesAcrossRuns(t *testing.T) {
	pipeline.ResetPreflightCache()

	dir := t.TempDir()
	fake := newFakeLLM(t, says("probe ok"), says("first"), says("second"))
	path := preflightPipeline(t, dir, fake.URL, "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)
	mustRun(t, path)

	// Three requests, not four: one probe shared by both runs, plus one
	// conversation turn each.
	if got := fake.requestCount(); got != 3 {
		t.Errorf("provider requests = %d, want 3 (the second run must trust the cached probe)", got)
	}
}

// TestNoPreflightFlagSkipsTheCheck covers the escape hatch.
func TestNoPreflightFlagSkipsTheCheck(t *testing.T) {
	pipeline.ResetPreflightCache()

	dir := t.TempDir()
	fake := newFakeLLM(t, says("done"))
	path := preflightPipeline(t, dir, fake.URL, "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{"run", path, "--job", "publish", "--no-preflight"})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	// One request: the conversation turn. No probe was made, which is the
	// only observable difference.
	if got := fake.requestCount(); got != 1 {
		t.Errorf("provider requests = %d, want 1 (--no-preflight must probe nothing)", got)
	}
}

// TestPerAgentPreflightOptOut covers the per-agent escape hatch, which exists
// for a model expected to be slow to WAKE — a cold local model would fail a
// probe that a real conversation would have waited out.
func TestPerAgentPreflightOptOut(t *testing.T) {
	pipeline.ResetPreflightCache()

	dir := t.TempDir()
	fake := newFakeLLM(t, says("done"))
	path := preflightPipeline(t, dir, fake.URL, "  preflight: false")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{"run", path, "--job", "publish"})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	if got := fake.requestCount(); got != 1 {
		t.Errorf("provider requests = %d, want 1 (an opted-out agent must not be probed)", got)
	}
}

// TestPreflightNamesTheEndpointContrast pins the diagnostic that a human
// reached for by hand: when one model on an endpoint answers and another does
// not, the same account, key, and endpoint are demonstrably fine — so the
// message must say so rather than leaving the reader to suspect credentials.
func TestPreflightNamesTheEndpointContrast(t *testing.T) {
	pipeline.ResetPreflightCache()

	dir := t.TempDir()

	// The fake answers in script order, and preflight probes agents in plan
	// order: `healthy` first, then `broken`.
	fake := newFakeLLM(t, says("probe ok"), failsWith(http.StatusInternalServerError))

	path := writePipeline(t, dir, fmt.Sprintf(`
agents:
- name: healthy
  source:
    endpoint: %[1]s/v1/
    model: good-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
- name: broken
  source:
    endpoint: %[1]s/v1/
    model: bad-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: publish
  plan:
  - agent: healthy
    inputs: []
    prompt: Plan it.
  - agent: broken
    inputs: []
    prompt: Build it.
`, fake.URL))

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{"run", path, "--job", "publish"})
	if err == nil {
		t.Fatal("run succeeded with one of its models down")
	}

	if !strings.Contains(err.Error(), "other models on this endpoint responded") {
		t.Errorf("error does not draw the contrast that identifies the model as the problem: %v", err)
	}
}

// TestPreflightCommandRunsNothing covers `steps preflight`: the same probe,
// asked deliberately, before committing to an hour-long run.
func TestPreflightCommandRunsNothing(t *testing.T) {
	pipeline.ResetPreflightCache()

	dir := t.TempDir()
	fake := newFakeLLM(t, says("probe ok"))
	path := preflightPipeline(t, dir, fake.URL, "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{"preflight", path, "--job", "publish"})
	if err != nil {
		t.Fatalf("preflight failed against a live model: %v", err)
	}

	assertNoFile(t, filepath.Join(dir, "task.log"))
}
