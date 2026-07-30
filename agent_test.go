package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunJobAgentNeverSkipped mirrors TestRunJobPutNeverSkipped's intent,
// counting provider requests since HTTP calls aren't file-shaped like that
// test's shell-builtin counters. Not run with t.Parallel(): it uses
// t.Setenv, which panics if called after a parallel test has started.
func TestRunJobAgentNeverSkipped(t *testing.T) {
	fake := newRepeatingFakeLLM(t, says("done"))

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipeline := fmt.Sprintf(`
agents:
- name: reviewer
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: build
  plan:
  - agent: reviewer
    inputs: []
    prompt: hello
`, fake.URL)

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	if got := fake.requestCount(); got != 1 {
		t.Errorf("calls after first run = %d, want 1", got)
	}

	mustRun(t, path)

	if got := fake.requestCount(); got != 2 {
		t.Errorf("calls after second run = %d, want 2 (agent steps are never skip-cached)", got)
	}
}

// TestRunJobAgentPromptFileArtifactReadsRepoFile is the end-to-end proof for
// the run-time prompt_file: {artifact, path} form: an agent step's prompt
// text is read out of a get step's fetched artifact and actually reaches the
// model. The dummy resource's in: writes PROMPT.md directly into the fetched
// directory (its cwd, per resource.RunIn), and the agent step declares repo
// as an input so it's materialized into its own working directory (see
// resolveDeferredPrompt) — matching how internal/workspace's
// checkPromptFileArtifactAvailable requires it to be declared.
func TestRunJobAgentPromptFileArtifactReadsRepoFile(t *testing.T) {
	fake := newRepeatingFakeLLM(t, says("done"))

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipeline := fmt.Sprintf(`
resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: echo 'Review this repo carefully.' > PROMPT.md

resources:
- name: repo
  type: dummy
  source: {}

agents:
- name: reviewer
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: build
  plan:
  - get: repo
  - agent: reviewer
    inputs: [repo]
    prompt_file: { artifact: repo, path: PROMPT.md }
`, fake.URL)

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	if got := fake.request(1).Messages; len(got) != 2 || !strings.Contains(got[1].Content, "Review this repo carefully.") {
		t.Errorf("the prompt_file's loaded text did not reach the model as the user message; got %+v", got)
	}
}
