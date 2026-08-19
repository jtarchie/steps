package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// TestEndToEndAgentWebFetch proves the web_fetch grant end to end: the tool
// reaches the wire under its own name, fetching an allowed URL hands the page
// body back to the model, and a URL outside the grant's allow: list comes
// back as tool-result data enforced by steps — not by prompt language the
// model could talk itself out of.
func TestEndToEndAgentWebFetch(t *testing.T) {
	dir := t.TempDir()

	content := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/spec" {
			http.NotFound(w, r)

			return
		}

		_, _ = fmt.Fprint(w, "The spec requires alt text on every image.")
	}))
	defer content.Close()

	fake := newFakeLLM(t,
		callsTool("web_fetch", map[string]any{"url": content.URL + "/spec"}),
		callsTool("web_fetch", map[string]any{"url": "https://untrusted.example/creds"}),
		callsTool("write_file", map[string]any{"path": "report/summary.md", "content": "alt text is required"}),
		says("Done."),
	)

	yaml := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: auditor
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools:
  - write_file
  - builtin: web_fetch
    allow: [127.0.0.1]

jobs:
- name: audit
  plan:
  - agent: auditor
    outputs: [report]
    prompt: Audit the site against the spec.
    assert:
      tool_calls:
      - name: web_fetch
`, fake.URL)

	path := writePipeline(t, dir, yaml)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	// ── wire layer ──────────────────────────────────────────────────────────
	// The grant compiled into a declaration the model can see, alongside the
	// other granted builtin.
	wantTools := []string{"write_file", "web_fetch"}
	if got := fake.request(1).toolNames(); !slices.Equal(got, wantTools) {
		t.Errorf("request 1 offered tools = %v, want %v", got, wantTools)
	}

	// ── tool-execution layer ────────────────────────────────────────────────
	// The allowed fetch returned the page body to the model.
	results := fake.request(2).toolResults()
	if len(results) != 1 || !strings.Contains(results[0], "alt text on every image") {
		t.Errorf("web_fetch of an allowed URL did not return the page body; got %v", results)
	}

	// The disallowed fetch was refused by steps and reported as data — the
	// error names the host and the allow: list, so the model can react.
	// toolResults returns the request's whole history; the refusal is the
	// newest entry.
	refused := fake.request(3).toolResults()
	if len(refused) != 2 || !strings.Contains(refused[1], "untrusted.example") || !strings.Contains(refused[1], "allow") {
		t.Errorf("web_fetch outside allow: was not refused as tool-result data; got %v", refused)
	}

	// ── store layer ─────────────────────────────────────────────────────────
	assertSucceeded(t, storeNodes(t, path), "agent", "auditor")
}
