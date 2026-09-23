package e2e

import (
	"encoding/json"
	"fmt"
	"testing"
)

// assistantReasoning reports, for each assistant message in a captured request, whether it carried a reasoning_content key.
func assistantReasoning(t *testing.T, raw string) []bool {
	t.Helper()

	var body struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}

	err := json.Unmarshal([]byte(raw), &body)
	if err != nil {
		t.Fatalf("decoding the captured request: %v", err)
	}

	var carried []bool

	for _, msg := range body.Messages {
		if string(msg["role"]) != `"assistant"` {
			continue
		}

		_, ok := msg["reasoning_content"]
		carried = append(carried, ok)
	}

	return carried
}

func contextPathsPipeline(t *testing.T, url, modelName string) string {
	t.Helper()

	return writePipeline(t, t.TempDir(), fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: reader
  source: {endpoint: %[1]s/v1/, model: %[2]s, api_key_env: STEPS_TEST_AGENT_API_KEY}

jobs:
- name: read
  plan:
  - task: seed
    outputs: [notes]
    run: echo 'always cite a line number' > notes/CONVENTIONS.md
  - agent: reader
    inputs: [notes]
    context_paths: [notes/CONVENTIONS.md]
    messages:
      - Summarize the conventions.
`, url, modelName))
}

// TestDeepSeekAssistantTurnsCarryReasoningContent is a DeepSeek model in thinking mode refusing a tool history whose assistant turns lack reasoning_content: context_paths: arrives as a synthetic read_file turn no model reasoned about, so the implementer's first request 400'd (measured through OpenCode, intermittently by upstream: without the key 1 in 3 failed, with an empty one 0 in 6).
func TestDeepSeekAssistantTurnsCarryReasoningContent(t *testing.T) {
	fake := newFakeLLM(t, says("Cite line numbers."))

	mustRun(t, contextPathsPipeline(t, fake.URL, "deepseek-v4-flash"))

	carried := assistantReasoning(t, fake.request(1).Raw)
	if len(carried) == 0 {
		t.Fatal("the first request held no assistant turn; want context_paths: injected as a synthetic tool exchange")
	}

	for i, ok := range carried {
		if !ok {
			t.Errorf("assistant message %d has no reasoning_content; a DeepSeek thinking-mode upstream rejects the request", i)
		}
	}
}

// TestOtherModelsSendNoReasoningContent is the control: OpenAI's own schema has no such field, so every other model's request is unchanged.
func TestOtherModelsSendNoReasoningContent(t *testing.T) {
	fake := newFakeLLM(t, says("Cite line numbers."))

	mustRun(t, contextPathsPipeline(t, fake.URL, "test-model"))

	for i, ok := range assistantReasoning(t, fake.request(1).Raw) {
		if ok {
			t.Errorf("assistant message %d carries reasoning_content for a non-DeepSeek model", i)
		}
	}
}

// TestDeepSeekReasoningIsSentBackVerbatim: DeepSeek requires the model's own reasoning_content back on every later request that carries tools, not merely the key; the empty value is only for turns no model produced.
func TestDeepSeekReasoningIsSentBackVerbatim(t *testing.T) {
	const thought = "The listing will show what exists."

	fake := newFakeLLM(t,
		turn{body: `{"id":"fake","object":"chat.completion","created":0,"model":"deepseek-v4-flash",
			"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
			"reasoning_content":` + mustJSON(thought) + `,
			"tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}}]}}]}`},
		says("Listed."),
	)

	path := writePipeline(t, t.TempDir(), fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: lister
  source: {endpoint: %s/v1/, model: deepseek-v4-flash, api_key_env: STEPS_TEST_AGENT_API_KEY}

jobs:
- name: list
  plan:
  - agent: lister
    inputs: []
    messages:
      - List the directory.
`, fake.URL))

	mustRun(t, path)

	var body struct {
		Messages []struct {
			Role             string  `json:"role"`
			ReasoningContent *string `json:"reasoning_content"`
		} `json:"messages"`
	}

	err := json.Unmarshal([]byte(fake.request(2).Raw), &body)
	if err != nil {
		t.Fatalf("decoding the second request: %v", err)
	}

	for _, msg := range body.Messages {
		if msg.Role != "assistant" {
			continue
		}

		if msg.ReasoningContent == nil || *msg.ReasoningContent != thought {
			t.Errorf("the model's tool-call turn went back with reasoning_content %v; want %q", msg.ReasoningContent, thought)
		}
	}
}
