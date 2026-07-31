package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// agentNodeResult returns the decoded nodes.result for the single agent step
// a test's run recorded.
func agentNodeResult(t *testing.T, pipelinePath string) map[string]any {
	t.Helper()

	st, err := store.OpenStore(statePath(pipelinePath))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	rows, err := st.ListNodes(context.Background(), "", 50)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}

	for _, row := range rows {
		if row.Kind != "agent" {
			continue
		}

		if row.Result == "" {
			t.Fatalf("agent node %q recorded no result", row.Resource)
		}

		var result map[string]any

		err = json.Unmarshal([]byte(row.Result), &result)
		if err != nil {
			t.Fatalf("decode node result: %v", err)
		}

		return result
	}

	t.Fatal("no agent node was recorded")

	return nil
}

// trajectoryNames returns the tool names from a recorded trajectory, in order.
func trajectoryNames(t *testing.T, result map[string]any) []string {
	t.Helper()

	raw, ok := result["trajectory"].([]any)
	if !ok {
		t.Fatalf("result has no trajectory: %v", result)
	}

	names := make([]string, 0, len(raw))

	for _, entry := range raw {
		call, isMap := entry.(map[string]any)
		if !isMap {
			t.Fatalf("trajectory entry is not an object: %v", entry)
		}

		name, _ := call["name"].(string)
		names = append(names, name)
	}

	return names
}

// What an agent actually did is now recorded. The trajectory used to live only
// in memory — for a routed-to successor and the handoff note — so the most
// useful question about an agent step had no answer once the run ended.
func TestAgentTrajectoryIsPersisted(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		callsTool("write_file", map[string]any{"path": "notes.md", "content": "some findings"}),
		callsTool("read_file", map[string]any{"path": "notes.md"}),
		says("Done."),
	)

	path := writePipeline(t, dir, `
agents:
- name: worker
  source:
    model: openai/test-model
    endpoint: `+fake.URL+`
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, write_file]
jobs:
- name: work
  plan:
  - agent: worker
    prompt: write some notes
`)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	result := agentNodeResult(t, path)

	got := trajectoryNames(t, result)
	want := []string{"write_file", "read_file"}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("trajectory = %v, want %v", got, want)
	}
}

// A FAILED agent step records its trajectory too. This is the case that
// matters: the step you need to reconstruct is the one that went wrong, and
// its work used to be discarded entirely.
func TestFailedAgentStepRecordsWhatItDid(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		callsTool("write_file", map[string]any{"path": "partial.md", "content": "half an answer"}),
		says("I could not finish."),
	)

	path := writePipeline(t, dir, `
agents:
- name: worker
  source:
    model: openai/test-model
    endpoint: `+fake.URL+`
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, write_file]
jobs:
- name: work
  plan:
  - agent: worker
    prompt: do the thing
    assert:
      stdout: "a phrase the model never says"
`)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{path})
	if err == nil {
		t.Fatal("expected the assert to fail the step")
	}

	result := agentNodeResult(t, path)

	if names := trajectoryNames(t, result); len(names) == 0 {
		t.Error("a failed agent step recorded no trajectory")
	}

	// The response it did produce is kept as well, not just the error.
	response, _ := result["response"].(string)
	if !strings.Contains(response, "could not finish") {
		t.Errorf("response = %q, want the text the model produced before failing", response)
	}
}

// An enormous tool argument is elided: the trajectory records what the agent
// did, not a second copy of everything it wrote.
func TestTrajectoryArgsAreTruncated(t *testing.T) {
	dir := t.TempDir()
	huge := strings.Repeat("x", 10_000)
	fake := newFakeLLM(t,
		callsTool("write_file", map[string]any{"path": "big.txt", "content": huge}),
		says("Written."),
	)

	path := writePipeline(t, dir, `
agents:
- name: worker
  source:
    model: openai/test-model
    endpoint: `+fake.URL+`
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [write_file]
jobs:
- name: work
  plan:
  - agent: worker
    prompt: write a big file
`)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	result := agentNodeResult(t, path)

	raw, _ := result["trajectory"].([]any)
	if len(raw) == 0 {
		t.Fatal("no trajectory recorded")
	}

	call, _ := raw[0].(map[string]any)
	args, _ := call["args"].(map[string]any)
	content, _ := args["content"].(string)

	if len(content) >= len(huge) {
		t.Errorf("recorded arg is %d bytes, want it truncated well below %d", len(content), len(huge))
	}

	if !strings.HasSuffix(content, "(truncated)") {
		t.Errorf("truncated arg does not say so: %q", content[max(0, len(content)-40):])
	}

	// The file itself is untouched — only the record of it is trimmed.
	written, err := os.ReadFile(filepath.Join(dir, "big.txt")) //nolint:gosec // path is a t.TempDir()-scoped file this test caused to be written
	if err == nil && len(written) != len(huge) {
		t.Errorf("the written file is %d bytes, want the full %d", len(written), len(huge))
	}
}

// --keep-workspace leaves a failed step's files on disk. A build workspace is
// otherwise removed unconditionally, including on the failure path, so the
// files a step had just edited when it broke — the first thing anyone would
// want to look at — were always gone before the error reached the terminal.
func TestKeepWorkspaceSurvivesAFailedStep(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: work
    run: |
      echo "work in progress" > artifact.txt
      exit 1
`)

	out := captureStdout(t, func() {
		err := run([]string{path, "--keep-workspace"})
		if err == nil {
			t.Fatal("expected the pipeline to fail")
		}
	})

	kept := keptWorkspaceDir(t, out)

	contents, err := os.ReadFile(filepath.Join(kept, "artifact.txt")) //nolint:gosec // path is parsed from this test's own run output
	if err != nil {
		t.Fatalf("the failed step's file was not kept: %v", err)
	}

	if !strings.Contains(string(contents), "work in progress") {
		t.Errorf("kept file = %q, want the failed step's output", contents)
	}

	// The test owns the kept directory now; nothing else will clean it up.
	t.Cleanup(func() { _ = os.RemoveAll(kept) })
}

// Without the flag, the workspace is removed as before.
func TestWorkspaceIsRemovedByDefault(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: work
    run: echo hi > artifact.txt
`)

	out := captureStdout(t, func() {
		err := run([]string{path})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	})

	if strings.Contains(out, "workspace kept") {
		t.Errorf("output = %q, want no kept-workspace line without the flag", out)
	}
}

// keptWorkspaceDir extracts the directory from the "workspace kept: <dir>"
// line the run printed.
func keptWorkspaceDir(t *testing.T, out string) string {
	t.Helper()

	for line := range strings.SplitSeq(out, "\n") {
		dir, found := strings.CutPrefix(strings.TrimSpace(line), "workspace kept: ")
		if found {
			return dir
		}
	}

	t.Fatalf("no kept-workspace line in output:\n%s", out)

	return ""
}
