package e2e

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestEndToEndACellAfterAnAgentRunsWhenTheAgentAnswersDifferently: a cell's cache key is its chain, and an agent's node hashes its configuration, not what it wrote — so a cell reading the agent's artifact kept its key when the artifact changed and was skipped "unchanged" against bytes it never saw.
func TestEndToEndACellAfterAnAgentRunsWhenTheAgentAnswersDifferently(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	seen := filepath.Join(dir, "seen.log")

	fake := newFakeLLM(t,
		callsTool("write_file", map[string]any{"path": "report/summary.md", "content": "first answer"}),
		says("Done."),
		callsTool("write_file", map[string]any{"path": "report/summary.md", "content": "second answer"}),
		says("Done."),
	)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: writer
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [write_file]

jobs:
- name: build
  plan:
  - agent: writer
    outputs: [report]
    messages:
      - Write the report.
  - across:
    - var: shard
      values: [a]
    task: read
    inputs: [report]
    run: |
      cat report/summary.md >> %[2]s
      echo >> %[2]s
`, fake.URL, seen))

	mustRun(t, "run", path, "--job", "build")
	mustRun(t, "run", path, "--job", "build")

	if got, want := readFileString(t, seen), "first answer\nsecond answer\n"; got != want {
		t.Errorf("the cell read %q over two runs, want %q: it must run again once the agent wrote something else", got, want)
	}
}
