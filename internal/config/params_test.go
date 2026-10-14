package config

import (
	"strings"
	"testing"
)

const paramsBase = `
mcp_servers:
- name: github
  endpoint: https://example.test/mcp
agents:
- name: helper
  description: helps
  source: { model: openrouter/x, api_key_env: K }
- name: worker
  source: { model: openrouter/x, api_key_env: K }
  tools:
  - TOOLS
jobs:
- name: build
  plan:
  - task: say
    run: echo hi
`

func TestParamsRefuseWhatCannotWork(t *testing.T) {
	t.Parallel()

	custom := func(params string) string {
		return "{name: t, description: d, run: 'echo {{ .args.q | shellquote }}', args: {pin: x}, params: " + params + "}"
	}

	for tool, want := range map[string]string{
		custom("{q: {min: 1}}"):                                        "is a string",
		custom("{q: {max_length: 3, min_length: 5}}"):                  "min_length 5 is above max_length 3",
		custom("{q: {pattern: '('}}"):                                  "does not compile",
		custom("{q: {max_length: -1}}"):                                ">= 0",
		custom("{q: {enum: []}}"):                                      "enum is empty",
		custom("{q: {max_length: 2, default: abc}}"):                   "default must be at most 2 characters",
		custom("{q: {enum: [a, b], default: c}}"):                      "default must be one of",
		custom("{nope: {max_length: 3}}"):                              "never reads {{ .args.nope }}",
		custom("{pin: {max_length: 3}}"):                               "pinned by args:",
		"{builtin: read_file, params: {path: {max_length: 3}}}":        "not valid on a built-in",
		"{mcp: github, params: {q: {max_length: 3}}}":                  "single-tool form",
		"{mcp: github, tool: x, params: {q: {max_length: 3, min: 1}}}": "a string or a number",
		"{mcp: github, tool: x, params: {q: {min: 5, max: 2}}}":        "min 5 is above max 2",
		"{agent: helper, params: {other: {max_length: 3}}}":            "takes one argument",
	} {
		_, err := Parse([]byte(strings.Replace(paramsBase, "- TOOLS", "- "+tool, 1)), "test", nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s\n= %v, want refused naming %q", tool, err, want)
		}
	}
}

func TestParamsOnAStepSelectionAreRefusedBecauseTheyWouldNotBind(t *testing.T) {
	t.Parallel()

	pipeline := `
agents:
- name: worker
  source: { model: openrouter/x, api_key_env: K }
  tools:
  - { name: t, description: d, run: 'echo {{ .args.q | shellquote }}' }
jobs:
- name: build
  plan:
  - agent: worker
    messages: [hi]
    tools:
    - { name: t, description: d, run: 'echo {{ .args.q | shellquote }}', params: { q: { max_length: 3 } } }
`
	_, err := Parse([]byte(pipeline), "test", nil)
	if err != nil {
		t.Fatalf("an inline custom tool defines its own spec, so its params bind: %v", err)
	}

	selection := strings.Replace(pipeline, "    - { name: t, description: d, run: 'echo {{ .args.q | shellquote }}', params: { q: { max_length: 3 } } }", "    - { mcp: github, tool: x, params: { q: { max_length: 3 } } }", 1)
	selection = "mcp_servers:\n- name: github\n  endpoint: https://example.test/mcp\n" + selection

	_, err = Parse([]byte(selection), "test", nil)
	if err == nil || !strings.Contains(err.Error(), "binds only where the tool is granted") {
		t.Errorf("a params: on a step's selection = %v, want refused", err)
	}
}

func TestParamsOnACustomSubAgentAndSingleToolGrantLoad(t *testing.T) {
	t.Parallel()

	for _, tool := range []string{
		"{name: t, description: d, run: 'echo {{ .args.q | shellquote }}', params: {q: {max_length: 10, pattern: '^a', default: abc}}}",
		"{agent: helper, params: {request: {max_length: 100}}}",
		"{mcp: github, tool: search, params: {limit: {min: 1, max: 50, default: '20'}, q: {enum: [a, b]}}}",
	} {
		_, err := Parse([]byte(strings.Replace(paramsBase, "- TOOLS", "- "+tool, 1)), "test", nil)
		if err != nil {
			t.Errorf("%s\n= %v, want it to load", tool, err)
		}
	}
}

func TestParamSpecChecksStringsAndNumbers(t *testing.T) {
	t.Parallel()

	maxLen, minLen := 3, 2
	low, high := 1.0, 5.0

	for _, tc := range []struct {
		name  string
		spec  ParamSpec
		str   string
		isNum bool
		num   float64
		want  string
	}{
		{name: "too long counts characters", spec: ParamSpec{MaxLength: &maxLen}, str: "héllo", want: "at most 3 characters (it was 5)"},
		{name: "multibyte within limit", spec: ParamSpec{MaxLength: &maxLen}, str: "héé"},
		{name: "too short", spec: ParamSpec{MinLength: &minLen}, str: "a", want: "at least 2 characters"},
		{name: "pattern unanchored", spec: ParamSpec{Pattern: "b"}, str: "abc"},
		{name: "pattern missed", spec: ParamSpec{Pattern: "^b"}, str: "abc", want: "must match the pattern ^b"},
		{name: "enum miss", spec: ParamSpec{Enum: []string{"a", "b"}}, str: "c", want: `must be one of: "a", "b"`},
		{name: "number under", spec: ParamSpec{Min: &low}, isNum: true, num: 0, want: "at least 1"},
		{name: "number over", spec: ParamSpec{Max: &high}, isNum: true, num: 5.5, want: "at most 5"},
		{name: "number in range", spec: ParamSpec{Min: &low, Max: &high}, isNum: true, num: 5},
		{name: "numeric enum", spec: ParamSpec{Enum: []string{"1", "2.5"}}, isNum: true, num: 2.5},
		{name: "numeric enum miss", spec: ParamSpec{Enum: []string{"1"}}, isNum: true, num: 2, want: "must be one of"},
	} {
		got := tc.spec.CheckString(tc.str)
		if tc.isNum {
			got = tc.spec.CheckNumber(tc.num)
		}

		if (tc.want == "" && got != "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
