package e2e

// An agent on volumes fetched its declared outputs home after every command, so an agent with outputs: [repo] paid a whole repository's round trip per edit (steps#246 measured 2m per GiB). These pin the replacement: one fetch when the step ends, and one before anything here reads the tree mid-step.

import (
	"fmt"
	"testing"
)

const fetchBlobBytes = 1 << 20

// TestPlacedAgentFetchesItsOutputsOnceNotPerWrite counts the bytes that came home: six writes to a 1 MiB output used to bring it home six times.
func TestPlacedAgentFetchesItsOutputsOnceNotPerWrite(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()

	calls := make([]turn, 0, 7)
	for i := range 5 {
		calls = append(calls, callsTool("write_file", map[string]any{"path": fmt.Sprintf("data/edit-%d.txt", i), "content": "edit\n"}))
	}

	calls = append(calls, callsTool("run_shell", map[string]any{"command": "echo shell >> data/edit-0.txt"}), says("edited"))
	fake := newFakeLLM(t, calls...)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: hand
  image: %[2]s
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [write_file, run_shell]

jobs:
- name: place
  plan:
  - task: prepare
    outputs: [data]
    run: head -c %[3]d /dev/urandom > data/blob

  - agent: hand
    tags: [box]
    inputs: [data]
    outputs: [data]
    messages:
      - Edit the tree.

  - task: check
    inputs: [data]
    run: test "$(cat data/edit-0.txt)" = "$(printf 'edit\nshell')" && test -s data/edit-4.txt && test -s data/blob
`, fake.URL, dockerE2EImage, fetchBlobBytes))

	mustRun(t, path, "--worker", "box=local:")

	row := placementNamed(t, runPlacements(t, path), "hand")
	if row.BytesReceived > 2*fetchBlobBytes {
		t.Errorf("bytes_received = %d, want about one %d-byte tree: the output came home once per write", row.BytesReceived, fetchBlobBytes)
	}
}

// TestPlacedAgentNudgeSeesWhatItWrote: a files: nudge reads this machine's copy mid-step, so the tree has to come home before it looks or the model is told to write a file it already wrote.
func TestPlacedAgentNudgeSeesWhatItWrote(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()

	fake := newFakeLLM(t,
		callsTool("write_file", map[string]any{"path": "data/report.md", "content": "done\n"}),
		says("wrote the report"),
		says("wrote the report, again"),
	)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: hand
  image: %[2]s
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [write_file]

jobs:
- name: place
  plan:
  - task: prepare
    outputs: [data]
    run: echo seed > data/seed.txt

  - agent: hand
    tags: [box]
    inputs: [data]
    outputs: [data]
    assert:
      files: [data/report.md]
      nudge: true
    messages:
      - Write data/report.md.
`, fake.URL, dockerE2EImage))

	mustRun(t, path, "--worker", "box=local:")

	if got := fake.requestCount(); got != 2 {
		t.Errorf("requests = %d, want 2 (write, then finish): the nudge did not see the file the agent wrote", got)
	}
}

// TestAnEditedInputComesHomeAsItsChanges: an input declared as an output came home whole after every command, so a one-line edit in a repository cost the repository (steps#246 S6). Only the overlay's upper layer travels now, and a deletion still reaches home.
func TestAnEditedInputComesHomeAsItsChanges(t *testing.T) {
	requireDockerE2E(t)

	path := writePipeline(t, t.TempDir(), fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

jobs:
- name: edit
  plan:
  - task: prepare
    outputs: [data]
    run: head -c %[2]d /dev/urandom > data/blob && mkdir -p data/sub && echo a > data/a.txt && echo b > data/sub/b.txt && echo c > data/sub/c.txt

  - task: change
    image: %[1]s
    inputs: [data]
    outputs: [data]
    run: echo more >> data/a.txt && rm data/sub/b.txt && echo new > data/sub/new.txt

  - task: check
    inputs: [data]
    run: test "$(cat data/a.txt)" = "$(printf 'a\nmore')" && test ! -e data/sub/b.txt && test -s data/sub/c.txt && test -s data/sub/new.txt && test -s data/blob
`, dockerE2EImage, fetchBlobBytes))

	mustRun(t, path)

	row := placementNamed(t, runPlacements(t, path), "change")
	if row.BytesReceived > fetchBlobBytes/4 {
		t.Errorf("bytes_received = %d, want about the edited files, not the %d-byte tree", row.BytesReceived, fetchBlobBytes)
	}
}
