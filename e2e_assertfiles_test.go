package main

// End-to-end coverage for the reaction an unmet assert.files: gets on an
// agent step: the model is told, at the moment it tries to stop, that the
// artifact it was asked for does not exist — and only fails if it will not
// comply.
//
// The failure this exists for was observed in production, not imagined: an
// agent answered a Slack thread in its final message instead of writing the
// file the pipeline was going to post, the `when:` guard downstream correctly
// found nothing to send, and the job reported SUCCEEDED with the thread left
// silent. Nothing in the run was red. That is the worst shape a pipeline
// failure can take, and a post-hoc assert cannot fix it — by the time it
// runs, the only party who could still write the file has gone home.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// assertFilesPipeline is an agent that must leave answer/reply.md behind, and
// a task that copies it out so the test can see what survived capture.
func assertFilesPipeline(t *testing.T, dir, endpoint string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: responder
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [write_file]

jobs:
- name: build
  plan:
  - agent: responder
    outputs: [answer]
    prompt: Answer the question. Write your answer to answer/reply.md.
    assert:
      files: [answer/reply.md]
  - task: deliver
    inputs: [answer]
    run: cat answer/reply.md >> %[2]s
`, endpoint, filepath.Join(dir, "delivered.log")))
}

// TestEndToEndAssertFilesNudgesBeforeFailing is the whole contract in one
// run: a model that tries to finish without its declared artifact is told
// what is missing and gets to fix it, and the step then succeeds normally.
//
// Turn 1 is the production failure exactly — the answer delivered as chat
// text, which reads like success and produces nothing.
func TestEndToEndAssertFilesNudgesBeforeFailing(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		says("Here is the answer: the catalog is seeded from widgets.json."),
		callsTool("write_file", map[string]any{
			"path":    "answer/reply.md",
			"content": "The catalog is seeded from widgets.json.",
		}),
		says("Written."),
	)
	path := assertFilesPipeline(t, dir, fake.URL)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	// The artifact exists, was captured, and reached the step downstream —
	// the whole point of the nudge is that the plan continues for real.
	if got := readFileString(t, filepath.Join(dir, "delivered.log")); !strings.Contains(got, "widgets.json") {
		t.Errorf("downstream task did not receive the agent's reply; got %q", got)
	}

	// The model was told WHICH path was missing, on the turn after it tried
	// to stop. A nudge that does not name the file is a nudge the model can
	// only guess at.
	if len(fake.requests) < 2 {
		t.Fatalf("provider saw %d requests, want at least 2 (the stop attempt and the nudged turn)", len(fake.requests))
	}

	nudged := fake.requests[1].Raw
	if !strings.Contains(nudged, "answer/reply.md") {
		t.Errorf("the nudged turn does not name the missing file; got %q", nudged)
	}

	nodes := storeNodes(t, path)

	assertSucceeded(t, nodes, "agent", "responder")
	assertSucceeded(t, nodes, "task", "deliver")
}

// TestEndToEndAssertFilesFailsAWillfulModel pins the other end: the nudge is
// a chance, not a loophole. A model that keeps answering in prose runs out of
// them and the step fails — which is what today's post-hoc assert already
// does, reached the same way it always was.
func TestEndToEndAssertFilesFailsAWillfulModel(t *testing.T) {
	dir := t.TempDir()
	fake := newRepeatingFakeLLM(t, says("The answer is in this message."))
	path := assertFilesPipeline(t, dir, fake.URL)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{path})
	if err == nil {
		t.Fatal("run() succeeded, but the agent never wrote its declared artifact")
	}

	if !strings.Contains(err.Error(), "answer/reply.md") {
		t.Errorf("failure does not name the missing file: %v", err)
	}

	// It gave up rather than looping forever, and it did try more than once
	// — a single attempt would mean the nudge never happened at all.
	if len(fake.requests) < 2 {
		t.Errorf("provider saw %d requests, want the stop attempt plus at least one nudge", len(fake.requests))
	}

	assertNoFile(t, filepath.Join(dir, "delivered.log"))
}
