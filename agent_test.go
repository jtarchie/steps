package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestRunJobAgentNeverSkipped is the one test that goes through a real HTTP
// round trip (via httptest.Server), since agent.RunStep constructs its
// OpenAI-compatible client internally rather than accepting an injectable
// one. It mirrors TestRunJobPutNeverSkipped's intent, using an in-memory
// request counter since HTTP calls aren't file-shaped like that test's
// shell-builtin counters. Not run with t.Parallel(): it uses t.Setenv,
// which panics if called after a parallel test has started.
func TestRunJobAgentNeverSkipped(t *testing.T) {
	var calls int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"id": "test-completion",
			"object": "chat.completion",
			"created": 0,
			"model": "test-model",
			"choices": [{"index": 0, "finish_reason": "stop", "logprobs": null, "message": {"role": "assistant", "content": "done"}}]
		}`)
	}))
	defer server.Close()

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
    prompt: hello
`, server.URL)

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	if calls != 1 {
		t.Errorf("calls after first run = %d, want 1", calls)
	}

	mustRun(t, path)

	if calls != 2 {
		t.Errorf("calls after second run = %d, want 2 (agent steps are never skip-cached)", calls)
	}
}
