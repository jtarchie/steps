package config

import (
	"strings"
	"testing"
)

const exportBase = `
mcp_servers:
- name: github
  endpoint: https://example.test/mcp
agents:
- name: oncall
  source: { model: openrouter/x, api_key_env: K }
jobs:
- name: build
  plan:
  - task: say
    run: echo hi
mcp_exports:
`

func TestAnExportRefusesWhatItCannotServe(t *testing.T) {
	t.Parallel()

	for export, want := range map[string]string{
		"- {name: a, access: 'true', tools: [read_file]}":                                                          "built-in",
		"- {name: a, access: 'true', tools: [{mcp: github, tool: x, max_calls: 2}]}":                               "rate: instead",
		"- {name: a, access: 'true', tools: [{name: t, run: 'echo hi', max_calls: 1, description: d}]}":            "rate: instead",
		"- {name: a, access: 'true', tools: [{name: t, run: 'echo hi', required: true, description: d}]}":          "required:",
		"- {name: a, access: 'true', tools: [{name: t, description: only a name}]}":                                "custom tool with a name and a run",
		"- {name: a, access: 'true', tools: [{name: 'a b', run: 'echo hi', description: d}]}":                      "not a tool name",
		"- {name: a, access: 'true', tools: [{agent: oncall, args: {x: y}}]}":                                      "sub-agent",
		"- {name: a, access: 'true', image: alpine, tools: [{mcp: github}]}":                                       "this export has none",
		"- {name: a, access: 'true', network: none, tools: [{name: t, run: 'echo hi', description: d}]}":           "requires image",
		"- {name: a, access: 'true', privileged: true, tools: [{name: t, run: 'echo hi', description: d}]}":        "requires image",
		"- {name: a, access: 'true', image: '-x', tools: [{name: t, run: 'echo hi', description: d}]}":             "must not start with '-'",
		"- {name: a, access: 'true', tools: [{mcp: github, tools: [x], args: {o: p}}]}":                            "single-tool form",
		"- {name: a, access: 'true', tools: [{mcp: nowhere}]}":                                                     "nowhere",
		"- {name: a, access: 'true', tools: [{agent: nobody}]}":                                                    "nobody",
		"- {name: a, access: 'true', tools: [{mcp: github}], rate: lots}":                                          "rate \"lots\"",
		"- {name: a, access: 'true', tools: []}":                                                                   "nothing to connect to",
		"- {name: a, access: 'true', tools: [{mcp: github}]}\n- {name: a, access: 'true', tools: [{mcp: github}]}": "declared twice",
		"- {name: 'a b', access: 'true', tools: [{mcp: github}]}":                                                  "name:",
		"- {name: a, access: 'true', tools: [{mcp: github, tool: x, env: [K]}]}":                                   "custom run: tool's command",
		"- {name: a, access: 'true', tools: [{agent: oncall, env: [K]}]}":                                          "custom run: tool's command",
		"- {name: a, access: 'true', tools: [{name: t, run: 'echo hi', description: d, env: ['K=v']}]}":            "NAME, not a KEY=VALUE",
		"- {name: a, access: 'true', tools: [{name: t, run: 'echo hi', description: d, env: [STEPS_RUN_ID]}]}":     "set by steps",
	} {
		_, err := Parse([]byte(exportBase+export), "test", nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s\n= %v, want refused naming %q", export, err, want)
		}
	}
}

func TestAnExportOfCommandsLoadsWithItsContainer(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(exportBase+"- {name: shell, access: 'true', image: alpine:3, user: nobody, network: none, container_limits: {memory: 1000000}, tools: [{name: say, description: d, run: 'echo {{ .args.x | shellquote }}', args: {y: z}, max_output_bytes: 100, timeout: 5s}]}"), "test", nil)
	if err != nil || cfg.MCPExports[0].Image != "alpine:3" || cfg.MCPExports[0].Limits == nil {
		t.Fatalf("Parse = %+v, %v; want the export loaded with its container", cfg, err)
	}
}

// env: on a tool exists for an export's command, whose environment nothing else sets; an agent's tools already share its env:.
func TestEnvOnAToolOutsideAnExportIsRefused(t *testing.T) {
	t.Parallel()

	for name, pipeline := range map[string]string{
		"agent grant": "agents:\n- name: a\n  source: { model: openrouter/x, api_key_env: K }\n  tools: [{name: t, description: d, run: 'echo hi', env: [K]}]\njobs:\n- name: j\n  plan:\n  - agent: a\n    messages: [hi]\n",
		"inline step": "agents:\n- name: a\n  source: { model: openrouter/x, api_key_env: K }\njobs:\n- name: j\n  plan:\n  - agent: a\n    messages: [hi]\n    tools: [{name: t, description: d, run: 'echo hi', env: [K]}]\n",
	} {
		_, err := Parse([]byte(pipeline), "test", nil)
		if err == nil || !strings.Contains(err.Error(), "agent's own env:") {
			t.Errorf("%s: Parse = %v, want env: on a tool refused, pointing at the agent's env:", name, err)
		}
	}

	cfg, err := Parse([]byte(exportBase+"- {name: shell, access: 'true', tools: [{name: t, description: d, run: 'echo hi', env: [LEVER_API_KEY]}]}"), "test", nil)
	if err != nil || len(cfg.MCPExports[0].Tools[0].Env) != 1 {
		t.Fatalf("Parse = %v; want an exported command's env: loaded", err)
	}
}

func TestAnExportOfGrantsAndAgentsLoads(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(exportBase+"- {name: support, access: 'true', rate: 60/m, tools: [{mcp: github, tool: search, args: {owner: acme}}, {mcp: github, tools: [get]}, {agent: oncall}]}"), "test", nil)
	if err != nil || len(cfg.MCPExports) != 1 || len(cfg.MCPExports[0].Tools) != 3 {
		t.Fatalf("Parse = %+v, %v; want the export loaded", cfg, err)
	}

	calls, window, err := ParseRate("60/m")
	if err != nil || calls != 60 || window.Minutes() != 1 {
		t.Errorf("ParseRate(60/m) = %d per %v, %v", calls, window, err)
	}
}

// TestAnExportUsesTheServersItShares: the mcp tab and steps mcp list say what reaches a server, and an export does.
func TestAnExportUsesTheServersItShares(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(exportBase+"- {name: support, access: 'true', tools: [{mcp: github}]}"), "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	if got := cfg.MCPUsers("github"); got != "export support" {
		t.Errorf("MCPUsers(github) = %q, want the export named", got)
	}
}
