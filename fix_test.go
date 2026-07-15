package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixAgentServer returns an httptest server that answers every chat
// completion with a plain "done" (no tool calls), counting how many times it
// was hit. A fix agent that just says "done" does no repair itself — so a
// task that recovers proves the control flow (fail -> invoke -> re-run),
// independent of the model's actual fixing ability.
func fixAgentServer(t *testing.T, calls *int) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"id": "test-completion",
			"object": "chat.completion",
			"created": 0,
			"model": "test-model",
			"choices": [{"index": 0, "finish_reason": "stop", "logprobs": null, "message": {"role": "assistant", "content": "done"}}]
		}`)
	}))
	t.Cleanup(server.Close)

	return server
}

func writeFixPipeline(t *testing.T, dir, endpoint, run string) string {
	t.Helper()

	path := filepath.Join(dir, "pipeline.yml")
	pipeline := fmt.Sprintf(`
agents:
- name: fixer
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, run_shell]

jobs:
- name: build
  plan:
  - task: check
    run: %s
    fix: fixer
`, endpoint, run)

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

// TestRunJobTaskFixRecovers: the task fails on its first run and passes on
// the re-run (a counter file makes the command fail-then-pass), so the fix
// agent is invoked exactly once and the job succeeds. Not parallel: uses
// t.Setenv.
func TestRunJobTaskFixRecovers(t *testing.T) {
	var calls int

	server := fixAgentServer(t, &calls)

	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	// Fail (exit 1) on the first invocation, pass (exit 0) on the second.
	run := fmt.Sprintf(`c=%s; n=$(cat "$c" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$c"; test $n -ge 2`, counter)
	path := writeFixPipeline(t, dir, server.URL, run)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	if calls != 1 {
		t.Errorf("fix agent calls = %d, want 1", calls)
	}

	if got := strings.TrimSpace(readFile(t, counter)); got != "2" {
		t.Errorf("counter = %q, want 2 (command should have run twice: initial + verdict re-run)", got)
	}
}

// TestRunJobTaskFixGreenPathSkipsAgent: a task that passes on its first run
// never constructs the fix agent — the $0 happy path.
func TestRunJobTaskFixGreenPathSkipsAgent(t *testing.T) {
	var calls int

	server := fixAgentServer(t, &calls)

	dir := t.TempDir()
	path := writeFixPipeline(t, dir, server.URL, "true")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	if calls != 0 {
		t.Errorf("fix agent calls = %d, want 0 (a passing task must not invoke the agent)", calls)
	}
}

// TestRunJobTaskFixStillFailing: a task that always fails invokes the agent
// once, but the verdict re-run still fails, so the job errors.
func TestRunJobTaskFixStillFailing(t *testing.T) {
	var calls int

	server := fixAgentServer(t, &calls)

	dir := t.TempDir()
	path := writeFixPipeline(t, dir, server.URL, "false")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{path})
	if err == nil {
		t.Fatal("expected the job to fail when the task still fails after the fix agent")
	}

	if calls != 1 {
		t.Errorf("fix agent calls = %d, want 1", calls)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // path is a t.TempDir()-scoped file this test wrote
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}
