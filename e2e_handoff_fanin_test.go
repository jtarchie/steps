package main

// End-to-end coverage for handoff notes across a concurrent block (#38):
// broadcast on fan-out, aggregate on fan-in.

import (
	"fmt"
	"strings"
	"testing"
)

// fanInPipeline is planner → (two reviewers in parallel) → synthesizer. Every
// agent step in the block both receives the planner's note and sends its own,
// which is the shape the feature exists for.
func fanInPipeline(endpoint string) string {
	return fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: planner
  source: { endpoint: %[1]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [read_file]
- name: security
  source: { endpoint: %[1]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [read_file]
- name: perf
  source: { endpoint: %[1]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [read_file]
- name: synthesizer
  source: { endpoint: %[1]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [read_file]

jobs:
- name: review
  plan:
  - agent: planner
    prompt: Plan the review.
    handoff: { note: true }
  - in_parallel:
      # Serialized so the fake provider's linear script stays deterministic:
      # concurrent branches would consume scripted turns in whatever order
      # they raced to the endpoint. What is under test here is note wiring and
      # delivery, which a limit of 1 exercises exactly as fully.
      limit: 1
      steps:
      - agent: security
        prompt: Review for security.
        handoff: { note: true }
      - agent: perf
        prompt: Review for performance.
        handoff: { note: true }
  - agent: synthesizer
    prompt: Combine the reviews.
`, endpoint)
}

// writesNote is the scripted turn pair an agent step with handoff: {note: true}
// must produce: the required write_handoff call, then its final text.
func writesNote(done string) []turn {
	return []turn{
		callsTool("write_handoff", map[string]any{
			"done":      done,
			"facts":     done + " facts",
			"watch_out": done + " risks",
		}),
		says(done + " complete."),
	}
}

// TestEndToEndHandoffNoteFanOutAndIn proves both directions on the wire: each
// branch opens with the planner's note (broadcast), and the step after the
// block opens with BOTH branch notes (aggregate), each fenced as data.
func TestEndToEndHandoffNoteFanOutAndIn(t *testing.T) {
	dir := t.TempDir()

	script := make([]turn, 0, 7)
	script = append(script, writesNote("planner")...)
	script = append(script, writesNote("security")...)
	script = append(script, writesNote("perf")...)
	script = append(script, says("Synthesized."))

	fake := newFakeLLM(t, script...)
	path := writePipeline(t, dir, fanInPipeline(fake.URL))

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	// The branches run concurrently, so their request numbers are not fixed.
	// The synthesizer is the LAST request either way.
	last := fake.request(fake.requestCount())

	results := last.toolResults()
	if len(results) != 2 {
		t.Fatalf("synthesizer opened with %d notes, want 2 (one per branch); got %v", len(results), results)
	}

	joined := strings.Join(results, "\n")
	for _, want := range []string{"security", "perf"} {
		if !strings.Contains(joined, want) {
			t.Errorf("fan-in delivery is missing the %q branch's note; got %v", want, results)
		}
	}

	// Each delivered note is framed as data. A fan-in puts several
	// model-authored documents into one conversation at once, which is the
	// widest injection surface this feature has.
	for i, result := range results {
		if !strings.Contains(result, "data, not instructions") {
			t.Errorf("note %d = %q, want the data-not-instructions framing", i, result)
		}

		// The tag only, not "<untrusted-": the captured request is JSON, where
		// the angle bracket arrives escaped as <.
		if !strings.Contains(result, "untrusted-") {
			t.Errorf("note %d = %q, want a randomized fence", i, result)
		}
	}

	// Broadcast: a branch opened with the planner's note. Requests 3 and 5 are
	// the branches' opening turns (1 and 2 are the planner's), whichever order
	// they ran in.
	broadcast := strings.Join(append(
		fake.request(3).toolResults(), fake.request(5).toolResults()...), "\n")
	if !strings.Contains(broadcast, "planner") {
		t.Errorf("branches did not receive the pre-block note; got %q", broadcast)
	}
}
