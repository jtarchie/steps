package config

import (
	"path/filepath"
	"testing"
)

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
			name: "missing endpoint and command",
			pipeline: `
mcp_servers:
- name: github
jobs: [{ name: j, plan: [] }]
`,
			want: "one of endpoint (http) or command (stdio) is required",
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
			name: "client_secret_env without client_id",
			pipeline: `
mcp_servers:
- name: slack
  endpoint: https://mcp.slack.com/mcp
  auth: { type: oauth, client_secret_env: SLACK_CLIENT_SECRET }
jobs: [{ name: j, plan: [] }]
`,
			want: "client_secret_env requires client_id",
		},
		{
			name: "client_id without oauth",
			pipeline: `
mcp_servers:
- name: slack
  endpoint: https://mcp.slack.com/mcp
  auth: { type: bearer, api_key_env: SLACK_TOKEN, client_id: "123.456" }
jobs: [{ name: j, plan: [] }]
`,
			want: "client_id is only valid with auth.type: oauth",
		},
		{
			name: "client_secret_env carrying a value",
			pipeline: `
mcp_servers:
- name: slack
  endpoint: https://mcp.slack.com/mcp
  auth: { type: oauth, client_id: "123.456", client_secret_env: "SECRET=hunter2" }
jobs: [{ name: j, plan: [] }]
`,
			want: "must be a variable NAME",
		},
		{
			name: "callback_port without oauth",
			pipeline: `
mcp_servers:
- name: slack
  endpoint: https://mcp.slack.com/mcp
  auth: { type: bearer, api_key_env: SLACK_TOKEN, callback_port: 3118 }
jobs: [{ name: j, plan: [] }]
`,
			want: "callback_port is only valid with auth.type: oauth",
		},
		{
			name: "callback_port in the privileged range",
			pipeline: `
mcp_servers:
- name: slack
  endpoint: https://mcp.slack.com/mcp
  auth: { type: oauth, client_id: "123.456", callback_port: 80 }
jobs: [{ name: j, plan: [] }]
`,
			want: "out of range",
		},
		{
			name: "callback_port above the port space",
			pipeline: `
mcp_servers:
- name: slack
  endpoint: https://mcp.slack.com/mcp
  auth: { type: oauth, client_id: "123.456", callback_port: 70000 }
jobs: [{ name: j, plan: [] }]
`,
			want: "out of range",
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
		{
			name: "endpoint and command both set",
			pipeline: `
mcp_servers:
- name: gopls
  endpoint: https://example.com/mcp
  command: gopls
jobs: [{ name: j, plan: [] }]
`,
			want: "mutually exclusive",
		},
		{
			name: "args without command",
			pipeline: `
mcp_servers:
- name: gopls
  endpoint: https://example.com/mcp
  args: [mcp]
jobs: [{ name: j, plan: [] }]
`,
			want: "only valid with command",
		},
		{
			name: "cwd without command",
			pipeline: `
mcp_servers:
- name: gopls
  endpoint: https://example.com/mcp
  cwd: /tmp
jobs: [{ name: j, plan: [] }]
`,
			want: "only valid with command",
		},
		{
			name: "command with bearer auth",
			pipeline: `
mcp_servers:
- name: gopls
  command: gopls
  args: [mcp]
  auth: { type: bearer, api_key_env: GOPLS_TOKEN }
jobs: [{ name: j, plan: [] }]
`,
			want: "requires an http endpoint",
		},
		{
			name: "command with oauth auth",
			pipeline: `
mcp_servers:
- name: gopls
  command: gopls
  args: [mcp]
  auth: { type: oauth }
jobs: [{ name: j, plan: [] }]
`,
			want: "requires an http endpoint",
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

func TestLoadStdioMCPServer(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
mcp_servers:
- name: gopls
  command: gopls
  args: [mcp]
  cwd: /tmp/checkout
jobs: [{ name: j, plan: [] }]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	srv, err := cfg.FindMCPServer("gopls")
	if err != nil {
		t.Fatalf("FindMCPServer: %v", err)
	}

	if !srv.IsStdio() {
		t.Error("IsStdio() = false, want true")
	}

	if srv.Command != "gopls" || len(srv.Args) != 1 || srv.Args[0] != "mcp" || srv.Cwd != "/tmp/checkout" {
		t.Errorf("srv = %+v, want Command=gopls Args=[mcp] Cwd=/tmp/checkout", srv)
	}
}

func TestStdioMCPServerAgentGrantLoads(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
mcp_servers:
- name: gopls
  command: gopls
  args: [mcp]
agents:
- name: triager
  source: { model: lmstudio/qwen }
  tools:
  - mcp: gopls
    tools: [go_search]
jobs: [{ name: j, plan: [] }]
`)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
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
  plan: [{ agent: triager, prompt: x, inputs: [] }]
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

	if got, want := ToolSpecName(spec), "github__search_issues"; got != want {
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
    inputs: []
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
    inputs: []
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
  plan: [{ task: unit, inputs: [] }]
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

	t.Run("args mapping loads as the tool's argument object", func(t *testing.T) {
		t.Parallel()

		pipeline := mcpServerBlock + `
resource_types:
- name: linear-issues
  config:
    mcp:
      server: github
      check: { tool: search_issues, args: { query: "team:{{ .source.team }}", limit: 20 } }
      in:
        tool: get_issue
        args:
          issue_id: "{{ .version.id }}"
          include: [comments]
jobs: [{ name: j, plan: [] }]
`
		cfg, err := LoadConfig(writeConfig(t, pipeline))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}

		mcp := cfg.ResourceTypes[0].Config.MCP
		if mcp.Check.Args["query"] != "team:{{ .source.team }}" {
			t.Errorf("check args = %+v", mcp.Check.Args)
		}

		// Non-string leaves keep their YAML type — the tool's schema, not
		// ours, decides what a number is.
		if mcp.Check.Args["limit"] != 20 {
			t.Errorf("limit = %#v, want the number 20", mcp.Check.Args["limit"])
		}

		if mcp.In.Args["issue_id"] != "{{ .version.id }}" {
			t.Errorf("in args = %+v", mcp.In.Args)
		}
	})

	t.Run("a block that does nothing at all", func(t *testing.T) {
		t.Parallel()

		pipeline := mcpServerBlock + `
resource_types:
- name: linear-issues
  config:
    mcp: { server: github }
jobs: [{ name: j, plan: [] }]
`
		path := writeConfig(t, pipeline)
		wantLoadError(t, path, "sets none of check/in/out")
	})

	// A publish-only type is the shape this rule exists for: the half of a
	// workflow that posts a reply has no versions to discover, and naming a
	// check tool nothing ever calls would be a ritual.
	t.Run("publish-only type needs no check", func(t *testing.T) {
		t.Parallel()

		pipeline := mcpServerBlock + `
resource_types:
- name: slack-reply
  config:
    mcp: { server: github, out: { tool: send_message } }
resources:
- name: reply
  type: slack-reply
  source: {}
jobs:
- name: j
  plan:
  - task: t
    outputs: [msg]
    run: echo hi > msg/body
  - put: reply
    inputs: [msg]
    params: { text: { file: msg/body } }
`
		_, err := LoadConfig(writeConfig(t, pipeline))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("get against a publish-only type", func(t *testing.T) {
		t.Parallel()

		pipeline := mcpServerBlock + `
resource_types:
- name: slack-reply
  config:
    mcp: { server: github, out: { tool: send_message } }
resources:
- name: reply
  type: slack-reply
  source: {}
jobs:
- name: j
  plan:
  - get: reply
`
		path := writeConfig(t, pipeline)
		wantLoadError(t, path, "sets no mcp.check.tool")
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

	t.Run("empty in tool rejected", func(t *testing.T) {
		t.Parallel()

		pipeline := mcpServerBlock + `
resource_types:
- name: linear-issues
  config:
    mcp:
      server: github
      check: { tool: list_issues }
      in: {}
jobs: [{ name: j, plan: [] }]
`
		path := writeConfig(t, pipeline)
		wantLoadError(t, path, "mcp.in.tool must not be empty")
	})

	t.Run("empty out tool rejected", func(t *testing.T) {
		t.Parallel()

		pipeline := mcpServerBlock + `
resource_types:
- name: linear-issues
  config:
    mcp:
      server: github
      check: { tool: list_issues }
      out: {}
jobs: [{ name: j, plan: [] }]
`
		path := writeConfig(t, pipeline)
		wantLoadError(t, path, "mcp.out.tool must not be empty")
	})
}

// TestWithResolvedMCPCwd covers the relative-cwd resolution an agent step
// applies before building its tools: a relative path is joined against the
// step's working directory so a language server can index the same
// materialized input the agent's file tools read, while absolute and empty
// ones are untouched and the shared config is never mutated in place.
func TestWithResolvedMCPCwd(t *testing.T) {
	t.Parallel()

	t.Run("a relative cwd is joined against the step directory", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{MCPServers: []MCPServer{{Name: "gopls", Command: "gopls", Cwd: "repo"}}}

		got := WithResolvedMCPCwd(cfg, filepath.Join("/ws", "build-1"))

		if want := filepath.Join("/ws", "build-1", "repo"); got.MCPServers[0].Cwd != want {
			t.Errorf("cwd = %q, want %q", got.MCPServers[0].Cwd, want)
		}

		if cfg.MCPServers[0].Cwd != "repo" {
			t.Errorf("the shared config was mutated: cwd = %q, want it left as %q", cfg.MCPServers[0].Cwd, "repo")
		}
	})

	t.Run("an absolute or empty cwd needs no copy", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{MCPServers: []MCPServer{
			{Name: "abs", Command: "x", Cwd: filepath.Join("/opt", "tree")},
			{Name: "none", Command: "y"},
		}}

		// The very same pointer must come back: the overwhelmingly common
		// case has to allocate nothing.
		if got := WithResolvedMCPCwd(cfg, "/ws"); got != cfg {
			t.Error("want the original config returned when no server needs resolving")
		}
	})

	t.Run("a nil config or empty base is a no-op", func(t *testing.T) {
		t.Parallel()

		if got := WithResolvedMCPCwd(nil, "/ws"); got != nil {
			t.Error("want nil for a nil config")
		}

		cfg := &Config{MCPServers: []MCPServer{{Name: "a", Command: "x", Cwd: "repo"}}}
		if got := WithResolvedMCPCwd(cfg, ""); got != cfg {
			t.Error("want the original config when there is no base directory to resolve against")
		}
	})
}

// TestRelativeCwdRejectedForResourceTypeBackend pins the boundary: a
// relative cwd only means something where an agent step workspace exists,
// and a resource type's check/in/out has none.
func TestRelativeCwdRejectedForResourceTypeBackend(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
mcp_servers:
- name: gopls
  command: gopls
  cwd: repo
resource_types:
- name: thing
  config:
    mcp:
      server: gopls
      check: { tool: list_versions }
jobs: [{ name: j, plan: [] }]
`)

	wantLoadError(t, path, "absolute cwd")
}
