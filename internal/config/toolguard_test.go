package config

import "testing"

func TestToolCallGuardsLoadAndResolve(t *testing.T) {
	t.Parallel()

	pipeline := `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - name: post_review
    description: Post a review verdict.
    run: gh pr review --repo {{ .args.repo | shellquote }} --{{ .args.action | shellquote }}
    required: true
    max_calls: 1
    args:
      repo: jtarchie/ci
jobs:
- name: review
  plan:
  - agent: reviewer
    inputs: []
    messages:
      - do it
`
	path := writeConfig(t, pipeline)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	ri, err := cfg.ResolveAgentInvocation(cfg.Jobs[0].Plan[0])
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	if len(ri.ToolSpecs) != 1 {
		t.Fatalf("resolved tools = %+v, want exactly one", ri.ToolSpecs)
	}

	got := ri.ToolSpecs[0]
	if got.MaxCalls != 1 {
		t.Errorf("MaxCalls = %d, want 1", got.MaxCalls)
	}

	if got.Args["repo"] != "jtarchie/ci" {
		t.Errorf("Args[repo] = %q, want %q", got.Args["repo"], "jtarchie/ci")
	}
}

func TestToolCallGuardsValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "max_calls on builtin",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - builtin: read_file
    max_calls: 2
jobs:
- name: j
  plan: [{ agent: reviewer, messages: [x], inputs: [] }]
`,
			want: "max_calls/args are only valid on custom tools",
		},
		{
			name: "args on sub-agent tool",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - agent: extra
    description: d
    args: { x: "1" }
- name: extra
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan: [{ agent: reviewer, messages: [x], inputs: [] }]
`,
			want: "max_calls/args are only valid on custom tools",
		},
		{
			name: "allow on a custom tool",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - name: post_review
    description: d
    run: echo hi
    allow: [example.com]
jobs:
- name: j
  plan: [{ agent: reviewer, messages: [x], inputs: [] }]
`,
			want: "allow is only valid on the web_fetch builtin",
		},
		{
			name: "allow on another builtin",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - builtin: run_shell
    allow: [example.com]
jobs:
- name: j
  plan: [{ agent: reviewer, messages: [x], inputs: [] }]
`,
			want: "allow is only valid on the web_fetch builtin",
		},
		{
			name: "negative max_calls",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - name: post_review
    description: d
    run: echo hi
    max_calls: -1
jobs:
- name: j
  plan: [{ agent: reviewer, messages: [x], inputs: [] }]
`,
			want: "max_calls must be >= 0",
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

// TestToolCallGuardsOnStepAndFix proves the guard validator walks step-level
// tools and fix: tool overrides too, not just agent grants.
func TestToolCallGuardsOnStepAndFix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "step-level builtin with max_calls",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools: [read_file]
jobs:
- name: j
  plan:
  - agent: reviewer
    inputs: []
    messages:
      - x
    tools:
    - builtin: read_file
      max_calls: 3
`,
			want: "max_calls/args are only valid on custom tools",
		},
		{
			name: "fix agent tool override with builtin max_calls",
			pipeline: `
agents:
- name: fixer
  source: { model: lmstudio/qwen }
  tools: [run_shell]
tasks:
- name: unit
  run: "true"
  fix:
    agent: fixer
    tools:
    - builtin: run_shell
      max_calls: 1
jobs:
- name: j
  plan: [{ task: unit, inputs: [] }]
`,
			want: "max_calls/args are only valid on custom tools",
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
