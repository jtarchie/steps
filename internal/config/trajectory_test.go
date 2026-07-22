package config

import "testing"

func TestAssertToolCallsLoads(t *testing.T) {
	t.Parallel()

	pipeline := `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - read_file
  - name: post_review
    description: Post a verdict.
    run: gh pr review --{{ .args.action | shellquote }}
jobs:
- name: review
  plan:
  - agent: reviewer
    inputs: []
    prompt: do it
    assert:
      stdout: posted
      tool_calls:
      - name: read_file
      - name: post_review
        args:
          action: comment
`
	path := writeConfig(t, pipeline)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	assert := cfg.Jobs[0].Plan[0].Assert
	if assert == nil {
		t.Fatal("assert did not decode")
	}

	if len(assert.ToolCalls) != 2 {
		t.Fatalf("tool_calls = %+v, want 2 entries", assert.ToolCalls)
	}

	if assert.ToolCalls[0].Name != "read_file" {
		t.Errorf("tool_calls[0].Name = %q, want read_file", assert.ToolCalls[0].Name)
	}

	if assert.ToolCalls[1].Args["action"] != "comment" {
		t.Errorf("tool_calls[1].Args[action] = %q, want comment", assert.ToolCalls[1].Args["action"])
	}
}

func TestAssertToolCallsValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "rejected on a task step",
			pipeline: `
jobs:
- name: j
  plan:
  - task: unit
    inputs: []
    run: "true"
    assert:
      tool_calls:
      - name: read_file
`,
			want: "assert.tool_calls is only valid on agent steps",
		},
		{
			name: "rejected on a get step",
			pipeline: `
resource_types:
- name: rt
  config: { check: "echo '[]'", in: "true" }
resources:
- name: r
  type: rt
  source: {}
jobs:
- name: j
  plan:
  - get: r
    assert:
      tool_calls:
      - name: read_file
`,
			want: "assert is not valid on get steps",
		},
		{
			name: "entry without a name",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: reviewer
    inputs: []
    prompt: x
    assert:
      tool_calls:
      - args: { a: "1" }
`,
			want: "name is required",
		},
		{
			name: "asserting on a pinned arg",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - name: post_review
    description: d
    run: gh pr review --repo {{ .args.repo }}
    args:
      repo: jtarchie/ci
jobs:
- name: j
  plan:
  - agent: reviewer
    inputs: []
    prompt: x
    assert:
      tool_calls:
      - name: post_review
        args:
          repo: jtarchie/ci
`,
			want: "can never match",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tc.pipeline)
			wantLoadError(t, path, tc.want)
		})
	}
}

// TestAssertToolCallsUnpinnedArgStillAllowed proves the pinned-arg check is
// narrow: asserting on a NON-pinned argument of a tool that pins some other
// argument is perfectly legal.
func TestAssertToolCallsUnpinnedArgStillAllowed(t *testing.T) {
	t.Parallel()

	pipeline := `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - name: post_review
    description: d
    run: gh pr review --repo {{ .args.repo }} --{{ .args.action }}
    args:
      repo: jtarchie/ci
jobs:
- name: j
  plan:
  - agent: reviewer
    inputs: []
    prompt: x
    assert:
      tool_calls:
      - name: post_review
        args:
          action: comment
`
	path := writeConfig(t, pipeline)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("asserting on a non-pinned arg should load fine, got: %v", err)
	}
}
