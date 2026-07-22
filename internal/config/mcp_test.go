package config

import "testing"

// mcpServerBlock is a minimal mcp_servers: entry reused across tests.
const mcpServerBlock = `
mcp_servers:
- name: github
  endpoint: https://api.githubcopilot.com/mcp/
  auth: { type: bearer, api_key_env: GITHUB_PAT }
`

func TestMCPServerValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "missing name",
			pipeline: `
mcp_servers:
- endpoint: https://example.com/mcp
jobs: [{ name: j, plan: [] }]
`,
			want: "name is required",
		},
		{
			name: "missing endpoint",
			pipeline: `
mcp_servers:
- name: github
jobs: [{ name: j, plan: [] }]
`,
			want: "endpoint is required",
		},
		{ //nolint:gosec // test fixture: the literal this validator must reject, not a real credential
			name: "userinfo in endpoint",
			pipeline: `
mcp_servers:
- name: github
  endpoint: https://user:token@example.com/mcp
jobs: [{ name: j, plan: [] }]
`,
			want: "must not embed credentials",
		},
		{
			name: "unknown auth type",
			pipeline: `
mcp_servers:
- name: github
  endpoint: https://example.com/mcp
  auth: { type: ninja }
jobs: [{ name: j, plan: [] }]
`,
			want: "auth.type must be one of",
		},
		{
			name: "bearer without api_key_env",
			pipeline: `
mcp_servers:
- name: github
  endpoint: https://example.com/mcp
  auth: { type: bearer }
jobs: [{ name: j, plan: [] }]
`,
			want: "requires api_key_env",
		},
		{
			name: "api_key_env without bearer",
			pipeline: `
mcp_servers:
- name: github
  endpoint: https://example.com/mcp
  auth: { type: oauth, api_key_env: GITHUB_PAT }
jobs: [{ name: j, plan: [] }]
`,
			want: "only valid with auth.type: bearer",
		},
		{
			name: "duplicate name",
			pipeline: `
mcp_servers:
- name: github
  endpoint: https://example.com/mcp
- name: github
  endpoint: https://example.com/mcp2
jobs: [{ name: j, plan: [] }]
`,
			want: "declared more than once",
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

func TestFindMCPServer(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, mcpServerBlock+"\njobs: [{ name: j, plan: [] }]\n")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	srv, err := cfg.FindMCPServer("github")
	if err != nil {
		t.Fatalf("FindMCPServer: %v", err)
	}

	if srv.Endpoint != "https://api.githubcopilot.com/mcp/" {
		t.Errorf("Endpoint = %q", srv.Endpoint)
	}

	_, err = cfg.FindMCPServer("ghost")
	if err == nil {
		t.Fatal("FindMCPServer(ghost): expected an error")
	}
}

// mcpAgentPipeline builds an agent granting an MCP tool per grantYAML
// (indented as a tools: list entry) plus a job invoking it.
func mcpAgentPipeline(grantYAML string) string {
	return mcpServerBlock + `
agents:
- name: triager
  source: { model: lmstudio/qwen }
  tools:
` + grantYAML + `
jobs:
- name: j
  plan: [{ agent: triager, prompt: x }]
`
}

func TestMCPToolGrantFormSingleTool(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, mcpAgentPipeline(`  - mcp: github
    tool: search_issues
    description: Search issues
    max_calls: 5
`))

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	ri, err := cfg.ResolveAgentInvocation(cfg.Jobs[0].Plan[0])
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	if len(ri.ToolSpecs) != 1 {
		t.Fatalf("ToolSpecs = %+v, want 1 entry", ri.ToolSpecs)
	}

	spec := ri.ToolSpecs[0]
	if spec.MCP != "github" || spec.MCPTool != "search_issues" {
		t.Fatalf("spec = %+v", spec)
	}

	if got, want := ToolSpecName(spec), "github.search_issues"; got != want {
		t.Errorf("ToolSpecName = %q, want %q", got, want)
	}
}

func TestMCPToolGrantFormNamedSubset(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, mcpAgentPipeline(`  - mcp: github
    tools: [get_issue, list_pulls]
`))

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	ri, err := cfg.ResolveAgentInvocation(cfg.Jobs[0].Plan[0])
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	spec := ri.ToolSpecs[0]
	if len(spec.MCPTools) != 2 {
		t.Fatalf("MCPTools = %+v", spec.MCPTools)
	}

	if got, want := ToolSpecName(spec), "github"; got != want {
		t.Errorf("ToolSpecName = %q, want %q", got, want)
	}
}

func TestMCPToolGrantFormAllTools(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, mcpAgentPipeline(`  - mcp: github
`))

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	ri, err := cfg.ResolveAgentInvocation(cfg.Jobs[0].Plan[0])
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	spec := ri.ToolSpecs[0]
	if spec.MCPTool != "" || len(spec.MCPTools) != 0 {
		t.Fatalf("spec = %+v, want neither tool nor tools set", spec)
	}

	if got, want := ToolSpecName(spec), "github"; got != want {
		t.Errorf("ToolSpecName = %q, want %q", got, want)
	}
}

func TestMCPToolGrantBareNameStepSelection(t *testing.T) {
	t.Parallel()

	pipeline := mcpServerBlock + `
agents:
- name: triager
  source: { model: lmstudio/qwen }
  tools:
  - read_file
  - mcp: github
    tools: [get_issue]
jobs:
- name: j
  plan:
  - agent: triager
    prompt: x
    tools: [github]
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

	if len(ri.ToolSpecs) != 1 || ri.ToolSpecs[0].MCP != "github" {
		t.Fatalf("bare-name selection resolved to %+v, want just the github mcp grant", ri.ToolSpecs)
	}
}

func TestMCPToolGrantValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		grant string
		want  string
	}{
		{
			name:  "unknown server",
			grant: "  - mcp: ghost\n    tool: x\n",
			want:  `no mcp_servers entry named "ghost"`,
		},
		{
			name:  "tool and tools both set",
			grant: "  - mcp: github\n    tool: x\n    tools: [y]\n",
			want:  "mutually exclusive",
		},
		{
			name:  "description on a subset grant",
			grant: "  - mcp: github\n    tools: [x]\n    description: d\n",
			want:  "only valid when tool: selects a single remote tool",
		},
		{
			name:  "required on the all-tools grant",
			grant: "  - mcp: github\n    required: true\n",
			want:  "only valid when tool: selects a single remote tool",
		},
		{
			name:  "args on a single-tool grant",
			grant: "  - mcp: github\n    tool: x\n    args: { a: b }\n",
			want:  "args is not valid on mcp tools",
		},
		{
			name:  "mixed with run",
			grant: "  - mcp: github\n    tool: x\n    run: echo hi\n",
			want:  "must not also set builtin/name/run/agent",
		},
		{
			name:  "negative max_calls",
			grant: "  - mcp: github\n    tool: x\n    max_calls: -1\n",
			want:  "max_calls must be >= 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, mcpAgentPipeline(tc.grant))
			wantLoadError(t, path, tc.want)
		})
	}
}

func TestMCPToolInlineOnStepRejected(t *testing.T) {
	t.Parallel()

	pipeline := mcpServerBlock + `
agents:
- name: triager
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: triager
    prompt: x
    tools:
    - mcp: github
      tool: search_issues
`
	path := writeConfig(t, pipeline)
	wantLoadError(t, path, "must be granted on an agent, not added inline on a step")
}

func TestMCPToolInFixIsAllowed(t *testing.T) {
	t.Parallel()

	// Unlike sub-agents, an MCP grant IS allowed in a fix agent's tools.
	pipeline := mcpServerBlock + `
agents:
- name: fixer
  source: { model: lmstudio/qwen }
  tools:
  - run_shell
  - mcp: github
    tool: search_issues
tasks:
- name: unit
  run: "true"
  fix: fixer
jobs:
- name: j
  plan: [{ task: unit }]
`
	path := writeConfig(t, pipeline)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
}

func TestResourceTypeMCPValidation(t *testing.T) {
	t.Parallel()

	t.Run("mutually exclusive with shell", func(t *testing.T) {
		t.Parallel()

		pipeline := mcpServerBlock + `
resource_types:
- name: linear-issues
  config:
    check: "echo []"
    mcp: { server: github, check: { tool: list_issues } }
jobs: [{ name: j, plan: [] }]
`
		path := writeConfig(t, pipeline)
		wantLoadError(t, path, "mutually exclusive")
	})

	t.Run("check tool required", func(t *testing.T) {
		t.Parallel()

		pipeline := mcpServerBlock + `
resource_types:
- name: linear-issues
  config:
    mcp: { server: github }
jobs: [{ name: j, plan: [] }]
`
		path := writeConfig(t, pipeline)
		wantLoadError(t, path, "mcp.check.tool is required")
	})

	t.Run("unresolvable server", func(t *testing.T) {
		t.Parallel()

		pipeline := `
resource_types:
- name: linear-issues
  config:
    mcp: { server: ghost, check: { tool: list_issues } }
jobs: [{ name: j, plan: [] }]
`
		path := writeConfig(t, pipeline)
		wantLoadError(t, path, `no mcp_servers entry named "ghost"`)
	})

	t.Run("loads with check only", func(t *testing.T) {
		t.Parallel()

		pipeline := mcpServerBlock + `
resource_types:
- name: linear-issues
  config:
    mcp: { server: github, check: { tool: list_issues } }
resources:
- name: eng-bugs
  type: linear-issues
  source: { team: ENG }
jobs:
- name: j
  plan: [{ get: eng-bugs, trigger: true }]
`
		path := writeConfig(t, pipeline)

		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("put rejected without out tool", func(t *testing.T) {
		t.Parallel()

		pipeline := mcpServerBlock + `
resource_types:
- name: linear-issues
  config:
    mcp: { server: github, check: { tool: list_issues } }
resources:
- name: eng-bugs
  type: linear-issues
  source: { team: ENG }
jobs:
- name: j
  plan: [{ put: eng-bugs, params: { title: x } }]
`
		path := writeConfig(t, pipeline)
		wantLoadError(t, path, "sets no mcp.out.tool")
	})

	t.Run("put allowed with out tool", func(t *testing.T) {
		t.Parallel()

		pipeline := mcpServerBlock + `
resource_types:
- name: linear-issues
  config:
    mcp:
      server: github
      check: { tool: list_issues }
      out: { tool: create_issue }
resources:
- name: eng-bugs
  type: linear-issues
  source: { team: ENG }
jobs:
- name: j
  plan: [{ put: eng-bugs, params: { title: x } }]
`
		path := writeConfig(t, pipeline)

		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})
}
