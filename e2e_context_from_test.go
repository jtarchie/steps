package main

// End-to-end coverage for context: { from: ... } — a step declaring which
// earlier steps' decisions it wants, and the obligation that demand creates on
// the step named.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// fromPipeline is a classifier followed by two readers: an agent that asked
// for the note, and a task that asked for the verdict. Neither is routed to —
// this is the plain fall-through case, which handoff: could never serve.
func fromPipeline(t *testing.T, dir, endpoint string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: triager
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
- name: filer
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: triage
  plan:
  - agent: triager
    inputs: []
    messages:
      - Classify the report.
    verdicts: [bug, feature]
  - agent: filer
    inputs: []
    messages:
      - File it.
    context:
      from:
        triager: note
  - task: record
    inputs: []
    context:
      from:
        triager: verdict
    run: cat upstream/triager >> %[2]s
`, endpoint, filepath.Join(dir, "record.log")))
}

// TestEndToEndContextFromDeliversDecisions proves the whole path: the demand
// forces the sender's note, the agent reader is handed it as a synthetic tool
// result before its own prompt, and the task reader gets it as a file.
func TestEndToEndContextFromDeliversDecisions(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		// triager decides, with the note its readers demanded
		callsTool("verdict", map[string]any{"choice": "bug", "note": "stack trace attached"}),
		says("Classified."),
		// filer answers having been handed that decision
		says("Filed under bugs."),
	)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")
	mustRun(t, fromPipeline(t, dir, fake.URL))

	// ── the demand became an obligation ───────────────────────────────────
	// note joined the verdict tool's required arguments, so the model cannot
	// satisfy the call without writing one.
	if want := `"required":["choice","note"]`; !strings.Contains(fake.request(1).Raw, want) {
		t.Errorf("verdict tool schema does not carry %s — the demand did not force the note", want)
	}

	// ── agent delivery ────────────────────────────────────────────────────
	// The reader's first request already contains the decision, as a
	// read_step exchange it never had to spend a turn asking for.
	reader := fake.request(3)
	for _, want := range []string{"read_step", "verdict: bug", "stack trace attached"} {
		if !strings.Contains(reader.Raw, want) {
			t.Errorf("reader's request does not carry %q", want)
		}
	}

	// It is fenced as untrusted data, like every other block of upstream
	// model-authored text that reaches a new model.
	if !strings.Contains(reader.Raw, "untrusted-") {
		t.Error("delivered decision is not fenced as data")
	}

	// ── task delivery ─────────────────────────────────────────────────────
	// The same decision, as a file the shell command read.
	if got := readFileString(t, filepath.Join(dir, "record.log")); !strings.Contains(got, "verdict: bug") {
		t.Errorf("task did not receive the decision as a file, got: %q", got)
	}
}

// TestEndToEndContextFromToleratesAnUnrunSender proves the revise-loop shape:
// a reader that runs BEFORE the step it reads from gets nothing rather than
// failing, and is handed the decision on the pass after.
//
// This is what replaces handoff:'s routed-entry-only rule. The question is
// asked of the run ("has that step decided yet?") rather than of the route,
// which is why it needs no relationship between the two steps at all.
func TestEndToEndContextFromToleratesAnUnrunSender(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		says("Drafted."),
		callsTool("verdict", map[string]any{"choice": "approve", "note": "good enough"}),
		says("Judged."),
	)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: writer
  source: {endpoint: %[1]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}
- name: critic
  source: {endpoint: %[1]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}

jobs:
- name: review
  plan:
  - agent: writer
    inputs: []
    messages:
      - Write it.
    context:
      from:
        critic: full
  - agent: critic
    inputs: []
    messages:
      - Judge it.
    verdicts:
      - approve
      - revise: writer
    max_visits: 2
`, endpoint(fake)))

	mustRun(t, path)

	// The writer ran first, so its request carries no decision at all — the
	// critic had not judged anything yet.
	if strings.Contains(fake.request(1).Raw, "read_step") {
		t.Error("the first pass delivered a decision from a step that had not run")
	}
}

// endpoint is a tiny readability shim: every fixture in this file points an
// agent at the same fake provider.
func endpoint(fake *fakeLLM) string { return fake.URL }

// TestEndToEndInjectedContextFollowsTheUserPrompt pins the ORDER of the
// messages a step's injected context produces. Both context_paths: files and
// an upstream decision arrive as synthetic tool exchanges, and a tool exchange
// only makes sense after a task has been given: an assistant that reaches for
// read_file before anyone asked it anything is a transcript no model ever saw
// in training, and some chat templates reject it outright (LM Studio's qwen3.8
// answers a conversation opening on an assistant tool call with
// "No user query found in messages").
func TestEndToEndInjectedContextFollowsTheUserPrompt(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		callsTool("verdict", map[string]any{"choice": "bug", "note": "stack trace attached"}),
		says("Classified."),
		says("Filed under bugs."),
	)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: triager
  source: {endpoint: %[1]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}
- name: filer
  source: {endpoint: %[1]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}

jobs:
- name: triage
  plan:
  - task: seed
    outputs: [notes]
    run: echo 'always cite a line number' > notes/CONVENTIONS.md
  - agent: triager
    inputs: []
    messages:
      - Classify the report.
    verdicts: [bug, feature]
  - agent: filer
    inputs: [notes]
    context_paths: [notes/CONVENTIONS.md]
    context:
      from:
        triager: note
    messages:
      - File it.
`, endpoint(fake)))

	mustRun(t, path)

	reader := fake.request(3)

	// Everything it was handed is still there...
	for _, want := range []string{"read_step", "stack trace attached", "always cite a line number"} {
		if !strings.Contains(reader.Raw, want) {
			t.Errorf("reader's request does not carry %q", want)
		}
	}

	// ...and the task it was handed them for comes first.
	if len(reader.Messages) < 2 {
		t.Fatalf("reader's request has %d messages, want at least 2", len(reader.Messages))
	}

	if got := reader.Messages[0].Role; got != "system" {
		t.Errorf("first message role = %q, want system", got)
	}

	if got := reader.Messages[1].Role; got != "user" {
		roles := make([]string, 0, len(reader.Messages))
		for _, msg := range reader.Messages {
			roles = append(roles, msg.Role)
		}

		t.Errorf("first non-system message role = %q, want user; got roles %v", got, roles)
	}

	if got := reader.Messages[1].Content; !strings.Contains(got, "File it.") {
		t.Errorf("first user message = %q, want the step's own prompt", got)
	}
}
