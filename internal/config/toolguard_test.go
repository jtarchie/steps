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
			name: "tool timeout is not a duration",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - name: post_review
    description: d
    run: echo hi
    timeout: soon
jobs:
- name: j
  plan: [{ agent: reviewer, messages: [x], inputs: [] }]
`,
			want: `invalid timeout "soon"`,
		},
		{
			name: "zero tool timeout",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - builtin: run_shell
    timeout: "0"
jobs:
- name: j
  plan: [{ agent: reviewer, messages: [x], inputs: [] }]
`,
			want: "timeout must be a positive duration",
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
			name: "step selection re-timing a granted builtin",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools: [run_shell]
jobs:
- name: j
  plan:
  - agent: reviewer
    inputs: []
    messages:
      - x
    tools:
    - builtin: run_shell
      timeout: 30s
`,
			want: "timeout: binds only where the tool is granted",
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

// TestToolTimeoutPositions pins where a per-call timeout: may be written.
// The two halves are one rule seen from both sides: a bare-name SELECTION is
// resolved by substituting the agent's own spec (resolveEffectiveTools), so a
// deadline written there would be dropped — while an inline custom tool is
// defined by the step, so its spec, deadline included, is what runs.
func TestToolTimeoutPositions(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - builtin: run_shell
    timeout: 5m
jobs:
- name: j
  plan:
  - agent: reviewer
    inputs: []
    messages:
      - x
    tools:
    - run_shell
    - name: tail_log
      description: d
      run: tail -f app.log
      timeout: 10s
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	ri, err := cfg.ResolveAgentInvocation(cfg.Jobs[0].Plan[0])
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	if len(ri.ToolSpecs) != 2 {
		t.Fatalf("resolved tools = %+v, want two", ri.ToolSpecs)
	}

	if got := ri.ToolSpecs[0].Timeout; got != "5m" {
		t.Errorf("selected builtin's timeout = %q, want 5m from the grant", got)
	}

	if got := ri.ToolSpecs[1].Timeout; got != "10s" {
		t.Errorf("inline custom tool's timeout = %q, want 10s", got)
	}
}

// TestCLIAgentToolTimeoutSplitsOnWhoRunsTheTool pins the half-rejection: a
// CLI-backed agent calls a custom or MCP tool through the bridge — the same
// impl the deadline is bound to, so it holds — but runs every built-in
// natively, where the bridge never sees the call and a deadline would be a
// fence that silently does not bind.
func TestCLIAgentToolTimeoutSplitsOnWhoRunsTheTool(t *testing.T) {
	t.Parallel()

	builtin := writeConfig(t, `
agents:
- name: coder
  source: { model: "@claude/sonnet" }
  tools:
  - builtin: run_shell
    timeout: 5m
jobs:
- name: j
  plan: [{ agent: coder, messages: [x], inputs: [] }]
`)
	wantLoadError(t, builtin, "sets timeout, which is not supported with a cli source")

	custom := writeConfig(t, `
agents:
- name: coder
  source: { model: "@claude/sonnet" }
  tools:
  - name: tail_log
    description: d
    run: tail -f app.log
    timeout: 5m
jobs:
- name: j
  plan: [{ agent: coder, messages: [x], inputs: [] }]
`)

	_, err := LoadConfig(custom)
	if err != nil {
		t.Errorf("LoadConfig rejected a bridged custom tool's timeout: %v", err)
	}
}
