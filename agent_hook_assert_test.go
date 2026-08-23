package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestAgentHookHonorsAssert pins the fix for the asymmetry between task and
// agent hooks: a task hook's assert: has always been evaluated (via
// internal/pipeline's executeTask -> runAssertedTask), while an agent hook's
// was parsed, validated at load, and then silently never run.
//
// config.validateAsserts deliberately walks hook steps, so an assert on an
// on_success: agent is checked and rejected at load exactly like a plan
// step's. Accepting it there and ignoring it at run time meant a hook whose
// whole job was to verify something reported success on the mismatch its
// assert existed to catch.
//
// The signal is binary on purpose: runHooks promotes a failing on_success
// hook to the job's error on an otherwise-green outcome, so before the fix
// this pipeline runs clean and after it fails, naming the assert.
func TestAgentHookHonorsAssert(t *testing.T) {
	t.Run("failing assert fails the hook", func(t *testing.T) {
		dir := t.TempDir()
		// The agent answers in prose and writes nothing, which is precisely
		// the failure assert.files exists to catch.
		//
		// Repeating rather than scripted: an unmet assert.files: is now put
		// back to the model when it tries to stop, so a model that refuses is
		// asked more than once by design (see maxFilesNudges). The refusal is
		// the fixture; how many times it takes to establish is not.
		fake := newRepeatingFakeLLM(t, says("All good, I have filed the incident note."))
		path := agentHookAssertPipeline(t, dir, fake.URL, "files: [note/incident.md]")

		t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

		err := run([]string{"run", path, "--job", "build"})
		if err == nil {
			t.Fatal("run() = nil, want a failure: the hook's assert.files names a file the agent never wrote")
		}

		if !strings.Contains(err.Error(), "assert.files") {
			t.Errorf("run() error = %q, want it to name the unsatisfied assert.files", err)
		}
	})

	// The control: the same pipeline whose assert the agent DOES satisfy has
	// to stay green, or the test above would pass on a hook that simply
	// always fails.
	t.Run("satisfied assert leaves the job green", func(t *testing.T) {
		dir := t.TempDir()
		fake := newFakeLLM(t,
			callsTool("write_file", map[string]any{
				"path":    "note/incident.md",
				"content": "the widget queue backed up at 03:00.",
			}),
			says("Incident note filed."),
		)
		path := agentHookAssertPipeline(t, dir, fake.URL, "files: [note/incident.md]")

		t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

		mustRun(t, "run", path, "--job", "build")
	})
}

// agentHookAssertPipeline writes a pipeline whose only agent runs as an
// on_success: hook carrying assertLine as its assert.
func agentHookAssertPipeline(t *testing.T, dir, endpoint, assertLine string) string {
	t.Helper()

	yaml := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: notifier
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [write_file]

jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: echo did the work
    on_success:
      agent: notifier
      inputs: []
      outputs: [note]
      messages:
        - File an incident note at note/incident.md.
      assert:
        %[2]s
`, endpoint, assertLine)

	return writePipeline(t, dir, yaml)
}
