package config

import (
	"fmt"
	"strings"
	"testing"
)

// subAgentPipeline is a minimal pipeline whose `reviewer` agent grants a
// sub-agent tool (`extra`), plus a job that invokes reviewer — enough to
// exercise validateAgentGraph and ResolveAgentInvocation. graphExtra lets a
// test append more agents/tools to build cycles and depth chains.
func subAgentPipeline(graphExtra string) string {
	return `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - read_file
  - agent: extra
    description: Delegate a narrow subtask.
- name: extra
  source: { model: lmstudio/qwen }
` + graphExtra + `
jobs:
- name: review
  plan:
  - agent: reviewer
    inputs: []
    messages:
      - do it
`
}

func TestSubAgentToolLoadsAndResolves(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, subAgentPipeline(""))

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	ri, err := cfg.ResolveAgentInvocation(cfg.Jobs[0].Plan[0])
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	// The granted sub-agent tool survives resolution, named by its agent.
	var found bool

	for _, tool := range ri.ToolSpecs {
		if tool.Agent == "extra" {
			found = true

			if ToolSpecName(tool) != "extra" {
				t.Errorf("ToolSpecName = %q, want %q", ToolSpecName(tool), "extra")
			}
		}
	}

	if !found {
		t.Fatalf("resolved tools %+v missing the sub-agent tool", ri.ToolSpecs)
	}
}

func TestSubAgentStepSelectsGrantedByBareName(t *testing.T) {
	t.Parallel()

	pipeline := `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - read_file
  - agent: extra
    description: Delegate.
- name: extra
  source: { model: lmstudio/qwen }
jobs:
- name: review
  plan:
  - agent: reviewer
    inputs: []
    messages:
      - do it
    tools: [extra]
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

	if len(ri.ToolSpecs) != 1 || ri.ToolSpecs[0].Agent != "extra" {
		t.Fatalf("bare-name selection resolved to %+v, want just the extra sub-agent", ri.ToolSpecs)
	}
}

func TestSubAgentValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "unknown sub-agent",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - agent: ghost
    description: nope
jobs:
- name: j
  plan: [{ agent: reviewer, messages: [x], inputs: [] }]
`,
			want: `no agent named "ghost"`,
		},
		{
			name: "sub-agent tool cannot be required",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - agent: extra
    description: d
    required: true
- name: extra
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan: [{ agent: reviewer, messages: [x], inputs: [] }]
`,
			want: "may not set required",
		},
		{
			name: "sub-agent tool mixed with run",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - agent: extra
    description: d
    run: echo hi
- name: extra
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan: [{ agent: reviewer, messages: [x], inputs: [] }]
`,
			want: "must not also set builtin/name/run",
		},
		{
			name: "inline sub-agent on a step",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
- name: extra
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: reviewer
    inputs: []
    messages:
      - x
    tools:
    - agent: extra
      description: d
`,
			want: "must be granted on an agent, not added inline on a step",
		},
		{
			name: "self cycle",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - agent: reviewer
    description: d
jobs:
- name: j
  plan: [{ agent: reviewer, messages: [x], inputs: [] }]
`,
			want: "agent cycle detected",
		},
		{
			name: "mutual cycle",
			pipeline: `
agents:
- name: a
  source: { model: lmstudio/qwen }
  tools: [{ agent: b, description: d }]
- name: b
  source: { model: lmstudio/qwen }
  tools: [{ agent: a, description: d }]
jobs:
- name: j
  plan: [{ agent: a, messages: [x], inputs: [] }]
`,
			want: "agent cycle detected",
		},
		{
			name: "fix agent grants sub-agent",
			pipeline: `
agents:
- name: fixer
  source: { model: lmstudio/qwen }
  tools:
  - run_shell
  - agent: extra
    description: d
- name: extra
  source: { model: lmstudio/qwen }
tasks:
- name: unit
  run: "true"
  fix: fixer
jobs:
- name: j
  plan: [{ task: unit, inputs: [] }]
`,
			want: "which a fix agent may not use",
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

func TestSubAgentDepthLimit(t *testing.T) {
	t.Parallel()

	// A chain a0 -> a1 -> ... -> a9 (10 agents) exceeds maxSubAgentDepth (8).
	var b strings.Builder

	b.WriteString("agents:\n")

	for i := range 10 {
		fmt.Fprintf(&b, "- name: a%d\n  source: { model: lmstudio/qwen }\n", i)

		if i < 9 {
			fmt.Fprintf(&b, "  tools: [{ agent: a%d, description: d }]\n", i+1)
		}
	}

	b.WriteString("jobs:\n- name: j\n  plan: [{ agent: a0, messages: [x], inputs: [] }]\n")

	path := writeConfig(t, b.String())
	wantLoadError(t, path, "nesting depth exceeded")
}

// TestSubAgentTreeSharing: a child that reaches the tree shares the parent's container, so one whose container would differ is refused at load (#246).
func TestSubAgentTreeSharing(t *testing.T) {
	t.Parallel()

	pipeline := func(parent, child string) string {
		return `
agents:
- name: lead
  source: { model: lmstudio/qwen }
` + parent + `
  tools:
  - agent: helper
    description: Delegate.
- name: helper
  source: { model: lmstudio/qwen }
` + child + `
jobs:
- name: j
  plan:
  - agent: lead
    messages: [x]
`
	}

	tests := map[string]struct {
		pipeline string
		want     string
	}{
		"same image":                    {pipeline("  image: busybox", "  image: busybox"), ""},
		"both on the host":              {pipeline("", ""), ""},
		"host child with its own fence": {pipeline("", "  sandbox: true"), ""},
		"different image":               {pipeline("  image: busybox", "  image: alpine"), `sub-agent "helper"`},
		"host child of an image parent": {pipeline("  image: busybox", ""), `sub-agent "helper"`},
		"different network":             {pipeline("  image: busybox", "  image: busybox\n  network: none"), `sub-agent "helper"`},
		"a child with no tree":          {pipeline("  image: busybox", "  image: alpine\n  tools: [web_fetch]"), ""},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tc.pipeline)
			if tc.want == "" {
				_, err := LoadConfig(path)
				if err != nil {
					t.Fatalf("LoadConfig: %v", err)
				}

				return
			}

			wantLoadError(t, path, tc.want)
		})
	}
}

// TestImageAgentRefusesRelativeMCPCwd: a stdio server runs on this machine, so a relative cwd: names a tree the containerized agent's edits never reach (#246).
func TestImageAgentRefusesRelativeMCPCwd(t *testing.T) {
	t.Parallel()

	pipeline := func(image, cwd string) string {
		return `
mcp_servers:
- name: lsp
  command: gopls
  cwd: ` + cwd + `
agents:
- name: bot
  source: { model: lmstudio/qwen }
` + image + `
  tools: [{ mcp: lsp }]
- name: fixer
  source: { model: lmstudio/qwen }
  tools: [{ mcp: lsp }]
jobs:
- name: j
  plan:
  - agent: bot
    messages: [x]
`
	}

	path := writeConfig(t, pipeline("  image: busybox", "repo"))
	wantLoadError(t, path, "cwd: repo")

	for _, ok := range []string{pipeline("", "repo"), pipeline("  image: busybox", "/abs/repo")} {
		_, err := LoadConfig(writeConfig(t, ok))
		if err != nil {
			t.Errorf("LoadConfig: %v", err)
		}
	}

	fix := `
mcp_servers:
- name: lsp
  command: gopls
  cwd: repo
agents:
- name: fixer
  source: { model: lmstudio/qwen }
  tools: [{ mcp: lsp }]
jobs:
- name: j
  plan:
  - task: t
    image: busybox
    run: "true"
    fix: fixer
`
	wantLoadError(t, writeConfig(t, fix), "cwd: repo")
}
