package e2e

// A sub-agent of an image agent edits the parent's copy of the tree: under volumes a container of its own started from what this machine last fetched, so a file the parent wrote outside outputs: was missing to it (#246).

import (
	"fmt"
	"testing"
)

func TestSubAgentSharesItsParentsContainer(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()

	fake := newFakeLLM(t,
		callsTool("write_file", map[string]any{"path": "scratch.txt", "content": "from the parent\n"}),
		callsTool("helper", map[string]any{"request": "copy it"}),
		callsTool("run_shell", map[string]any{"command": "cp scratch.txt data/out.txt"}),
		says("copied"),
		says("done"),
	)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: lead
  image: %[2]s
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools:
  - write_file
  - agent: helper
    description: Delegate.
- name: helper
  image: %[2]s
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]

jobs:
- name: j
  plan:
  - task: prepare
    outputs: [data]
    run: echo seed > data/seed.txt

  - agent: lead
    inputs: [data]
    outputs: [data]
    messages:
      - Have the helper copy scratch.txt into data/.

  - task: check
    inputs: [data]
    run: test "$(cat data/out.txt)" = "from the parent"
`, fake.URL, dockerE2EImage))

	mustRun(t, path)
}
