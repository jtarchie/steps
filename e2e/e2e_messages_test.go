package e2e

// A step that holds a conversation rather than asking one question.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// messagesPipeline is an agent step with two user turns.
func messagesPipeline(t *testing.T, dir, endpoint string) string {
	t.Helper()

	return writePipeline(t, dir, `
defaults:
  preflight:
    disabled: true

agents:
- name: reviewer
  source:
    model: openai/test-model
    endpoint: `+endpoint+`/v1/
    api_key_env: STEPS_TEST_AGENT_API_KEY
  system: You review code.
  tools: [read_file]

jobs:
- name: review
  plan:
  - agent: reviewer
    messages:
      - "Review the diff."
      - "Name the line your verdict turns on."
`)
}

// TestEndToEndSecondMessageIsSentAfterTheFirstIsAnswered is the feature: the
// pipeline holds more than one turn, and a later message reaches the model only
// once the earlier one has been answered.
//
// The ordering is the point. Both messages arriving in the opening request
// would be two questions asked at once — the model composing its answer to the
// second while still deciding the first — which is exactly what writing them as
// one prompt already does, and not what this is for.
func TestEndToEndSecondMessageIsSentAfterTheFirstIsAnswered(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		says("The diff looks fine."),
		says("Line 42 of parser.go."),
	)
	path := messagesPipeline(t, dir, fake.URL)
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	if got := fake.requestCount(); got != 2 {
		t.Fatalf("provider saw %d requests, want one per message", got)
	}

	// The opening request carries the first message and nothing of the second.
	first := fake.request(1)
	if !first.userMessageContains("Review the diff.") {
		t.Error("the first request did not carry the first message")
	}

	if first.userMessageContains("Name the line") {
		t.Error("the second message was sent before the first was answered — both arrived at once")
	}

	// The second request carries both: it is the same conversation, continued.
	second := fake.request(2)
	if !second.userMessageContains("Name the line") {
		t.Error("the second message never reached the model")
	}

	if !second.userMessageContains("Review the diff.") {
		t.Error("the second request lost the first message — this is a continuation, not a fresh conversation")
	}
}

// TestEndToEndOneMessageIsUnchanged pins that the common case did not move: a
// single message is one request, exactly as a single prompt: always was.
func TestEndToEndOneMessageIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t, says("Looks fine."))

	path := writePipeline(t, dir, `
defaults:
  preflight:
    disabled: true

agents:
- name: reviewer
  source:
    model: openai/test-model
    endpoint: `+fake.URL+`/v1/
    api_key_env: STEPS_TEST_AGENT_API_KEY
  system: You review code.

jobs:
- name: review
  plan:
  - agent: reviewer
    messages:
      - "Review the diff."
`)
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	if got := fake.requestCount(); got != 1 {
		t.Fatalf("provider saw %d requests, want exactly 1", got)
	}
}

// TestEndToEndMessagesAndMessageFilesAreRefused pins the rule at the level a
// pipeline author meets it: two ordered lists cannot say which message comes
// first, so declaring both is refused at load rather than resolved by a
// precedence nobody would guess.
func TestEndToEndMessagesAndMessageFilesAreRefused(t *testing.T) {
	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "review.md"), []byte("Review the diff.\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	path := writePipeline(t, dir, `
defaults:
  preflight:
    disabled: true

agents:
- name: reviewer
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }

jobs:
- name: review
  plan:
  - agent: reviewer
    messages: ["inline"]
    message_files: [review.md]
`)

	runErr := cli.Run([]string{"validate", "--syntax-only", path})
	if runErr == nil {
		t.Fatal("a step declaring both messages: and message_files: loaded")
	}

	if !strings.Contains(runErr.Error(), "mutually exclusive") {
		t.Errorf("error = %v, want it to say the two are mutually exclusive", runErr)
	}
}
