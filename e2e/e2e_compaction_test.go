package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// compactedReviewPipeline renders one agent whose first tool result (a ~2KB file)
// pushes the conversation past a tiny compact_after_tokens:, so the next turn
// opens with a summary request.
func compactedReviewPipeline(t *testing.T, dir, endpoint, extra string) string {
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
  tools: [read_file]
  compact_after_tokens: 50
%[2]s

jobs:
- name: review
  plan:
  - task: prep
    inputs: []
    outputs: [prep]
    run: |
      mkdir -p prep
      printf '%%2000s' | tr ' ' x > prep/NOTES.txt
  - agent: reviewer
    inputs: [prep]
    messages:
      - Review the notes.
`, endpoint, extra))
}

// TestCompactionIsVisibleAndCounted is #162 end to end: a compaction is
// marked in the recorded transcript with the summary the model worked from,
// the summary request's tokens are in the step's spend, and the step's
// result counts it.
func TestCompactionIsVisibleAndCounted(t *testing.T) {
	dir := t.TempDir()

	fake := newFakeLLM(t,
		callsTool("read_file", map[string]any{"path": "prep/NOTES.txt"}).spending(10),
		says("SUMMARY-MARKER: read NOTES.txt, all x.").spending(1000),
		says("Done.").spending(100),
	)

	path := compactedReviewPipeline(t, dir, fake.URL, "")

	err := cli.Run([]string{"run", path, "--job", "review"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := fake.requestCount(); got != 3 {
		t.Fatalf("provider requests = %d, want 3 (turn, summary, turn)", got)
	}

	if !strings.Contains(fake.request(2).systemMessage(), "You are summarizing") {
		t.Errorf("request 2 is not the summary request: %q", fake.request(2).systemMessage())
	}

	// The seam: the summary was SENT ON, not only recorded.
	if !fake.request(3).userMessageContains("[Previous conversation summary]") ||
		!fake.request(3).userMessageContains("SUMMARY-MARKER") {
		t.Error("the turn after compaction was not handed the summary")
	}

	assertOneCompactionMarker(t, path)

	if got := agentUsageFor(t, path).Total; got != 1110 {
		t.Errorf("recorded usage = %d, want 1110 — the summary request's 1000 tokens are spend", got)
	}

	assertCompactionsRecorded(t, path)
}

// TestCompactionSummaryCrossingTheBudgetStopsTheStep: a summary always has a
// request behind it, so crossing the agent's budget stops the step before
// anything more is spent — and the summary it never used is not marked.
func TestCompactionSummaryCrossingTheBudgetStopsTheStep(t *testing.T) {
	dir := t.TempDir()

	fake := newFakeLLM(t,
		callsTool("read_file", map[string]any{"path": "prep/NOTES.txt"}).spending(10),
		says("SUMMARY-MARKER").spending(1000),
		says("Done.").spending(100),
	)

	path := compactedReviewPipeline(t, dir, fake.URL, "  budget:\n    tokens: 500")

	err := cli.Run([]string{"run", path, "--job", "review"})
	if err == nil || !strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("err = %v, want a budget breach", err)
	}

	if got := fake.requestCount(); got != 2 {
		t.Errorf("provider requests = %d, want 2 — nothing after the summary that crossed the budget", got)
	}

	if got := agentUsageFor(t, path).Total; got != 1010 {
		t.Errorf("recorded usage = %d, want 1010", got)
	}

	if strings.Contains(storeTranscript(t, path, "reviewer"), `"type":"compaction"`) {
		t.Error("the transcript marks a compaction the model never worked from")
	}
}

// assertOneCompactionMarker holds the recorded transcript to exactly one
// compaction event carrying the summary and a label saying what it did.
func assertOneCompactionMarker(t *testing.T, path string) {
	t.Helper()

	var transcript []struct {
		Type string `json:"type"`
		Name string `json:"name"`
		Text string `json:"text"`
	}

	err := json.Unmarshal([]byte(storeTranscript(t, path, "reviewer")), &transcript)
	if err != nil {
		t.Fatalf("decode transcript: %v", err)
	}

	var markers int

	for _, event := range transcript {
		if event.Type != "compaction" {
			continue
		}

		markers++

		if !strings.Contains(event.Text, "SUMMARY-MARKER") || !strings.Contains(event.Name, "messages summarized") {
			t.Errorf("compaction event = %+v, want the summary and a label saying what was summarized", event)
		}
	}

	if markers != 1 {
		t.Errorf("transcript carries %d compaction events, want 1: %+v", markers, transcript)
	}
}

// assertCompactionsRecorded holds the step's recorded result to counting one
// compaction that did not stall.
func assertCompactionsRecorded(t *testing.T, path string) {
	t.Helper()

	var result map[string]any

	err := json.Unmarshal([]byte(storeNodeResult(t, path, "reviewer")), &result)
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}

	if result["compactions"] != float64(1) {
		t.Errorf("result compactions = %v, want 1", result["compactions"])
	}

	if _, ok := result["compaction_stalled"]; ok {
		t.Error("result says compaction stalled; it did not")
	}
}
