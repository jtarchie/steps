package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// TestRunJSCallsGrantedToolsAsTheStepsOwn runs a script that calls a granted tool three times in one model turn, and holds the three things that make a script's call the step's own: the tool's max_calls: refuses the third, its required: is met without a turn spent forcing it, and the trajectory names every call.
func TestRunJSCallsGrantedToolsAsTheStepsOwn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	llm := newFakeLLM(t,
		callsTool("run_js", map[string]any{"code": `
const first = mark({});
mark({});
const third = mark({});
({first: first.stdout.trim(), third: third.error})
`}),
		says("Marked."),
	)

	yaml := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: scripter
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools:
  - run_js
  - name: mark
    description: Mark that the required step ran.
    run: echo marked
    required: true
    max_calls: 2

jobs:
- name: build
  plan:
  - agent: scripter
    messages:
      - Mark it.
    assert:
      stdout: Marked.
`, llm.URL)

	path := writePipeline(t, dir, yaml)

	mustRun(t, path)

	if got := llm.requestCount(); got != 2 {
		t.Fatalf("provider requests = %d, want 2: a required: tool a script called must not be forced again", got)
	}

	second := llm.request(2)
	if !second.toolResultContains(`"first":"marked"`) {
		t.Errorf("the script's value did not reach the model; tool results: %v", second.toolResults())
	}

	if !second.toolResultContains("call budget (2) exhausted") {
		t.Errorf("the third mark() was not refused by max_calls: 2; tool results: %v", second.toolResults())
	}

	result := storeNodeResult(t, path, "scripter")
	if got := strings.Count(result, `"name":"mark"`); got != 3 {
		t.Errorf(`recorded trajectory calls "mark" %d times, want 3 (two run, one refused); result: %s`, got, result)
	}

	if !strings.Contains(result, `"name":"run_js","ok":true`) {
		t.Errorf("the run_js call itself is not recorded as succeeded, so its result was never marked; result: %s", result)
	}

	transcript := storeTranscript(t, path, "scripter")
	for _, event := range []string{"call", "result"} {
		if got := strings.Count(transcript, `"type":"`+event+`","name":"mark"`); got != 3 {
			t.Errorf("transcript narrates %d mark %ss, want the script's 3; transcript: %s", got, event, transcript)
		}
	}
}

// TestRunJSScriptCallsAreRecordedWhereTheyRan puts run_js in the middle of a turn of three calls. The loop records the turn before running any of it, so the script's call has to be filed behind its own run_js and ahead of the call the model made after it: assert.tool_calls: reads the order, and the order it ran in is run_js, first, second.
func TestRunJSScriptCallsAreRecordedWhereTheyRan(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	llm := newFakeLLM(t,
		callsTools(
			call("zero", map[string]any{}),
			call("run_js", map[string]any{"code": `first({}).exit_code`}),
			call("second", map[string]any{}),
		),
		says("Done."),
	)

	yaml := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: scripter
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools:
  - run_js
  - {name: zero, description: Zero., run: echo zero}
  - {name: first, description: First., run: echo first}
  - {name: second, description: Second., run: echo second}

jobs:
- name: build
  plan:
  - agent: scripter
    messages:
      - Run them in order.
    assert:
      stdout: Done.
      tool_calls:
      - name: run_js
      - name: first
      - name: second
`, llm.URL)

	path := writePipeline(t, dir, yaml)

	mustRun(t, path)

	result := storeNodeResult(t, path, "scripter")

	at := func(name string) int { return strings.Index(result, `"name":"`+name+`"`) }
	if at("zero") < 0 || at("zero") >= at("run_js") || at("run_js") >= at("first") || at("first") >= at("second") {
		t.Errorf("recorded order is not zero, run_js, first, second; result: %s", result)
	}

	if strings.Count(result, `"ok":true`) != 4 {
		t.Errorf("want all four calls marked succeeded, each by its own result; result: %s", result)
	}
}
