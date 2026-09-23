package e2e

// pr-review run KHOH5SVQCGSUXVLE: six reviewers hit max_turns unwarned, the tools-withheld wrap-up could not write a file, and five of six left no report.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

const wrapUpNudge = "then your tools are taken away"

func wrapUpPipeline(t *testing.T, dir, endpoint string, maxTurns int) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: reviewer
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell, write_file]
  max_turns: %[3]d

jobs:
- name: build
  plan:
  - agent: reviewer
    outputs: [answer]
    messages:
      - Investigate, then write answer/reply.md.
    assert:
      files: [answer/reply.md]
  - task: deliver
    inputs: [answer]
    run: cat answer/reply.md >> %[2]s
`, endpoint, filepath.Join(dir, "delivered.log"), maxTurns))
}

// The nudge has to arrive while the tools are still granted, or the file it asks for cannot be written.
func TestTurnCapNudgesTheModelToWriteItsDeliverable(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": "true"}),
		callsTool("run_shell", map[string]any{"command": "true"}),
		callsTool("run_shell", map[string]any{"command": "true"}),
		callsTool("write_file", map[string]any{"path": "answer/reply.md", "content": "what I found"}),
		says("Written."),
	)

	mustRun(t, wrapUpPipeline(t, dir, fake.URL, 5))

	if got := readFileString(t, filepath.Join(dir, "delivered.log")); !strings.Contains(got, "what I found") {
		t.Errorf("the deliverable did not reach the next step; got %q", got)
	}

	if fake.request(3).userMessageContains(wrapUpNudge) {
		t.Error("nudged with three of five turns left")
	}

	fourth := fake.request(4)
	if !fourth.userMessageContains(wrapUpNudge) {
		t.Error("no nudge with two of five turns left")
	}

	if len(fourth.toolNames()) == 0 {
		t.Error("the nudge withdrew the tools; the model could not have written its file")
	}
}

// A wrap-up answer is not the declared deliverable, so a capped step without it is red rather than green and empty.
func TestTurnCapWithoutTheDeliverableIsRed(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": "true"}),
		callsTool("run_shell", map[string]any{"command": "true"}),
		says("Here is everything I found, as prose."),
	)

	err := cli.Run([]string{"run", wrapUpPipeline(t, dir, fake.URL, 2), "--job", "build"})
	if err == nil {
		t.Fatal("a capped step that never wrote its declared file went green")
	}

	if !strings.Contains(err.Error(), "answer/reply.md") {
		t.Errorf("the failure does not name the missing file: %v", err)
	}

	assertNoFile(t, filepath.Join(dir, "delivered.log"))
}
