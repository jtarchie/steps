package e2e

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

	"github.com/jtarchie/steps/internal/cli"
)

// assertFilesPipeline is an agent that must leave answer/reply.md behind, and
// a task that copies it out so the test can see what survived capture. nudge
// is whether the step opts into being told while it can still act.
func assertFilesPipeline(t *testing.T, dir, endpoint string, nudge bool) string {
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
    messages:
      - Answer the question. Write your answer to answer/reply.md.
    assert:
      files: [answer/reply.md]
      nudge: %[3]t
  - task: deliver
    inputs: [answer]
    run: cat answer/reply.md >> %[2]s
`, endpoint, filepath.Join(dir, "delivered.log"), nudge))
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
	path := assertFilesPipeline(t, dir, fake.URL, true)

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
	path := assertFilesPipeline(t, dir, fake.URL, true)

	err := cli.Run([]string{path})
	if err == nil {
		t.Fatal("cli.Run succeeded, but the agent never wrote its declared artifact")
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

// TestEndToEndAssertFilesWithoutNudgeFailsAtOnce pins that the nudge is
// opt-in: the SAME script that the nudged run rescues — prose first, the
// file on the turn after — fails on the prose when the step never asked to
// be told. Without this, deleting "write it to answer/reply.md" from a
// prompt could never be caught by a real-model fixture, because the nudge
// would rescue the model every time.
//
// The failure also says how to opt in, so the change from always-nudging
// reads as a one-line migration rather than a mystery regression.
func TestEndToEndAssertFilesWithoutNudgeFailsAtOnce(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		says("Here is the answer: the catalog is seeded from widgets.json."),
		callsTool("write_file", map[string]any{
			"path":    "answer/reply.md",
			"content": "The catalog is seeded from widgets.json.",
		}),
		says("Written."),
	)
	path := assertFilesPipeline(t, dir, fake.URL, false)

	err := cli.Run([]string{path})
	if err == nil {
		t.Fatal("cli.Run succeeded, but the step never opted into a nudge and the model answered in prose")
	}

	if !strings.Contains(err.Error(), "answer/reply.md") {
		t.Errorf("failure does not name the missing file: %v", err)
	}

	if !strings.Contains(err.Error(), "nudge: true") {
		t.Errorf("failure does not say how to opt into being told: %v", err)
	}

	if len(fake.requests) != 1 {
		t.Errorf("provider saw %d requests, want exactly 1: nothing may put the model back without the flag", len(fake.requests))
	}

	assertNoFile(t, filepath.Join(dir, "delivered.log"))
}

// assertToolCallsPipeline is an agent that must run the tests before it
// finishes — a procedure, where assertFilesPipeline declares a deliverable.
func assertToolCallsPipeline(t *testing.T, dir, endpoint string, nudge bool) string {
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
  tools:
  - name: run_tests
    description: Run the test suite.
    run: echo "tests pass" >> %[2]s

jobs:
- name: build
  plan:
  - agent: reviewer
    messages:
      - Review the change. Run the tests before you answer.
    assert:
      tool_calls:
      - name: run_tests
      nudge: %[3]t
`, endpoint, filepath.Join(dir, "tests.log"), nudge))
}

// TestEndToEndAssertToolCallsNudgesBeforeFailing is the seam for the second
// obligation: a model that answers without following the declared procedure
// is told which calls it still owes, makes them, and the step succeeds.
func TestEndToEndAssertToolCallsNudgesBeforeFailing(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		says("Looks good to me."),
		callsTool("run_tests", map[string]any{}),
		says("Tests pass; looks good to me."),
	)
	path := assertToolCallsPipeline(t, dir, fake.URL, true)

	mustRun(t, path)

	if got := readFileString(t, filepath.Join(dir, "tests.log")); !strings.Contains(got, "tests pass") {
		t.Errorf("the tool never ran; got %q", got)
	}

	if len(fake.requests) < 2 {
		t.Fatalf("provider saw %d requests, want at least 2 (the stop attempt and the nudged turn)", len(fake.requests))
	}

	nudged := fake.requests[1].Raw
	if !strings.Contains(nudged, "run_tests") {
		t.Errorf("the nudged turn does not name the tool still owed; got %q", nudged)
	}

	// Names only: an argument value would be dictating the call, and the
	// files wording ("final message is not the deliverable") corrects a
	// belief a procedure gap does not involve.
	if strings.Contains(nudged, "final message is not the deliverable") {
		t.Errorf("a tool_calls nudge borrowed the files wording; got %q", nudged)
	}

	assertSucceeded(t, storeNodes(t, path), "agent", "reviewer")
}

// TestEndToEndAssertToolCallsWithoutNudgeFailsAtOnce is the opt-in half for
// tool_calls, over the same script.
func TestEndToEndAssertToolCallsWithoutNudgeFailsAtOnce(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		says("Looks good to me."),
		callsTool("run_tests", map[string]any{}),
		says("Tests pass; looks good to me."),
	)
	path := assertToolCallsPipeline(t, dir, fake.URL, false)

	err := cli.Run([]string{path})
	if err == nil {
		t.Fatal("cli.Run succeeded, but the model never ran the tests and the step never opted into a nudge")
	}

	if !strings.Contains(err.Error(), "assert.tool_calls") || !strings.Contains(err.Error(), "run_tests") {
		t.Errorf("failure does not name the assert and the tool: %v", err)
	}

	if len(fake.requests) != 1 {
		t.Errorf("provider saw %d requests, want exactly 1", len(fake.requests))
	}

	assertNoFile(t, filepath.Join(dir, "tests.log"))
}
