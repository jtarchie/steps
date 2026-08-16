package main

// End-to-end coverage for the step output cache: the claim that an unchanged
// expensive step costs nothing the second time, and that what it produced is
// still there for the steps below it.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// stepCachePipeline is a plan whose middle step is the expensive one: a get
// feeds an agent, and a task downstream reads what the agent wrote.
//
// The agent declares no verdicts:, no to:, no hooks and no context: from:, so
// it is cacheable — the shape the whole pr-review fan-out has. The publish task
// is volatile:, so it runs on EVERY pass regardless of the cache: without that
// it would be reused too, and a restored report/ artifact nobody reads proves
// nothing about restoration.
//
// One fake provider serves every run of a test, and its URL is baked in here:
// the endpoint is part of an agent's hashed content, so a second provider on a
// second port would be a different step and would miss the cache for a reason
// that has nothing to do with what is under test.
func stepCachePipeline(t *testing.T, dir, endpoint, root, publishLog, fetchLog, notes string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy
  root: %[2]s

defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: |
      echo '%[5]s' > NOTES.txt
      echo fetched >> %[4]s

resources:
- name: repo
  type: dummy
  source: {}

agents:
- name: reviewer
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, write_file]

jobs:
- name: build
  plan:
  - get: repo
  - agent: reviewer
    inputs: [repo]
    outputs: [report]
    prompt: Review the notes and summarize them.
  - task: publish
    volatile: true
    inputs: [report]
    run: |
      cat report/summary.md >> %[3]s
      echo >> %[3]s
  assert:
    execution: [repo, reviewer, publish]
    outcome: succeeded
`, endpoint, root, publishLog, fetchLog, notes))
}

// stepCacheScript is the conversation the reviewer has when it actually runs.
func stepCacheScript() []turn {
	return []turn{
		callsTool("read_file", map[string]any{"path": "repo/NOTES.txt"}),
		callsTool("write_file", map[string]any{
			"path":    "report/summary.md",
			"content": "widgetd seeds its catalog from widgets.json.",
		}),
		says("Done."),
	}
}

// stepCacheScriptTimes repeats the script, for a test whose agent is expected
// to run more than once against one provider.
func stepCacheScriptTimes(n int) []turn {
	script := make([]turn, 0, n*len(stepCacheScript()))
	for range n {
		script = append(script, stepCacheScript()...)
	}

	return script
}

const stepCacheNotes = "The widget catalog is seeded from widgets.json on boot."

// TestStepCacheReusesAnAgentStep is the feature's headline claim: rerunning a
// pipeline whose inputs did not change costs no model calls, and the artifact
// the reused step produced is still delivered to the step that reads it.
func TestStepCacheReusesAnAgentStep(t *testing.T) {
	dir, root := t.TempDir(), t.TempDir()
	publishLog := filepath.Join(dir, "publish.log")
	fetchLog := filepath.Join(dir, "fetch.log")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	fake := newFakeLLM(t, stepCacheScript()...)
	path := stepCachePipeline(t, dir, fake.URL, root, publishLog, fetchLog, stepCacheNotes)

	mustRun(t, "test", path)

	if got := fake.requestCount(); got != len(stepCacheScript()) {
		t.Fatalf("provider requests on the first run = %d, want %d", got, len(stepCacheScript()))
	}

	if got := readFileString(t, publishLog); !strings.Contains(got, "widgetd seeds its catalog") {
		t.Fatalf("the first run's publish step did not see the agent's report; got %q", got)
	}

	// Second pass, same root: the agent's content and the bytes of its one
	// declared input are unchanged, so its entry is found and no model is
	// asked anything. The script is exhausted, so a request would also fail
	// the fake — the count is the assertion that says why.
	mustRun(t, "test", path)

	if got := fake.requestCount(); got != len(stepCacheScript()) {
		t.Errorf("provider requests after the rerun = %d, want %d — the rerun asked the model %d more time(s)",
			got, len(stepCacheScript()), got-len(stepCacheScript()))
	}

	// The volatile: task ran again, and the report it read came out of the
	// cache rather than out of a conversation.
	assertLineCount(t, publishLog, 2)

	if got := readFileString(t, publishLog); strings.Count(got, "widgetd seeds its catalog") != 2 {
		t.Errorf("the reused agent step's report did not reach the second run's publish step; got %q", got)
	}

	// The get is not cached — a resource fetch has its own cache, opted into
	// separately — so a reused agent step does not silently freeze the rest of
	// the plan.
	assertLineCount(t, fetchLog, 2)

	// The reused agent node says so. Without this a succeeded agent node with
	// no transcript and no tokens is indistinguishable from one that ran and
	// produced nothing.
	assertSucceeded(t, storeNodes(t, path), "agent", "reviewer")

	if got := storeNodeResult(t, path, "reviewer"); !strings.Contains(got, `"reused":true`) {
		t.Errorf("the reused agent node's result = %q, want it to record that it was reused", got)
	}
}

// TestStepCacheRerunsWhenAnInputChanges is the other half of the contract: the
// key is the input BYTES, so a changed upstream artifact re-runs the step even
// though the step's own declaration is untouched.
func TestStepCacheRerunsWhenAnInputChanges(t *testing.T) {
	dir, root := t.TempDir(), t.TempDir()
	publishLog := filepath.Join(dir, "publish.log")
	fetchLog := filepath.Join(dir, "fetch.log")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	fake := newFakeLLM(t, stepCacheScriptTimes(2)...)

	mustRun(t, "test", stepCachePipeline(t, dir, fake.URL, root, publishLog, fetchLog, stepCacheNotes))

	// Same plan, same agent, same endpoint — only what the get: fetches is
	// different, which is a change no plan-time hash of the agent step can
	// see.
	changed := "The widget catalog is seeded from catalog.toml on boot."
	mustRun(t, "test", stepCachePipeline(t, dir, fake.URL, root, publishLog, fetchLog, changed))

	if got := fake.requestCount(); got != 2*len(stepCacheScript()) {
		t.Errorf("provider requests across both runs = %d, want %d (different input bytes are different work)",
			got, 2*len(stepCacheScript()))
	}
}

// TestStepCacheVolatileAgentAlwaysRuns pins volatile: on the kind of step it
// exists for — one whose answer must never be replayed.
func TestStepCacheVolatileAgentAlwaysRuns(t *testing.T) {
	dir, root := t.TempDir(), t.TempDir()
	publishLog := filepath.Join(dir, "publish.log")
	fetchLog := filepath.Join(dir, "fetch.log")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	fake := newFakeLLM(t, stepCacheScriptTimes(2)...)

	path := stepCachePipeline(t, dir, fake.URL, root, publishLog, fetchLog, stepCacheNotes)
	volatileAgent := strings.Replace(
		readFileString(t, path),
		"  - agent: reviewer\n",
		"  - agent: reviewer\n    volatile: true\n",
		1,
	)
	path = writePipeline(t, dir, volatileAgent)

	mustRun(t, "test", path)
	mustRun(t, "test", path)

	if got := fake.requestCount(); got != 2*len(stepCacheScript()) {
		t.Errorf("provider requests across both runs = %d, want %d (volatile: must never be reused)",
			got, 2*len(stepCacheScript()))
	}
}
