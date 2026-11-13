package e2e

// A fix: agent repairs the failing task's own tree: it shares the task's container rather than building one of its own, which under volumes (#246) would be a second copy whose edits the re-run never sees.

import (
	"fmt"
	"strings"
	"testing"
)

// TestAFixAgentWithDirWorksBelowTheTaskAndRerunsItAtTheRoot pins both halves of dir: on a fix agent: the model's tools work in the subdirectory, and the rerun tool is the task's own command, run where the task runs it.
func TestAFixAgentWithDirWorksBelowTheTaskAndRerunsItAtTheRoot(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()

	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": "pwd"}),
		callsTool("check", map[string]any{}),
		callsTool("write_file", map[string]any{"path": "fixed", "content": "yes\n"}),
		says("fixed"),
	)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: fixer
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [write_file, run_shell]

jobs:
- name: build
  plan:
  - task: seed
    outputs: [out]
    run: echo seed > out/seed
  - task: check
    image: %[2]s
    inputs: [out]
    outputs: [out]
    run: pwd; test -f out/fixed
    fix:
      agent: fixer
      dir: out
`, fake.URL, dockerE2EImage))

	mustRun(t, path)

	shell := strings.TrimSpace(lastToolResult(t, fake.request(2)))
	if !strings.Contains(shell, "/out") {
		t.Errorf("the fix agent's run_shell pwd = %q, want it in dir: out", shell)
	}

	rerun := lastToolResult(t, fake.request(3))
	if strings.Contains(rerun, "/out\\n") || strings.Contains(rerun, "/out\n") {
		t.Errorf("the rerun tool ran in %q, want the task's own directory, not dir: out", rerun)
	}

	assertSucceeded(t, storeNodes(t, path), "task", "check")
}
