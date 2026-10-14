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
		"- {name: a, access: 'true', tools: [{mcp: github, tool: x, max_output_bytes: 2}]}":                        "workspace",
		"- {name: a, access: 'true', tools: [{mcp: github, tools: [x], args: {o: p}}]}":                            "single-tool form",
		"- {name: a, access: 'true', tools: [{mcp: nowhere}]}":                                                     "nowhere",
		"- {name: a, access: 'true', tools: [{agent: nobody}]}":                                                    "nobody",
		"- {name: a, access: 'true', tools: [{mcp: github}], rate: lots}":                                          "rate \"lots\"",
		"- {name: a, access: 'true', tools: []}":                                                                   "nothing to connect to",
		"- {name: a, access: 'true', tools: [{mcp: github}]}\n- {name: a, access: 'true', tools: [{mcp: github}]}": "declared twice",
		"- {name: 'a b', access: 'true', tools: [{mcp: github}]}":                                                  "name:",
	} {
		_, err := Parse([]byte(exportBase+export), "test", nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s\n= %v, want refused naming %q", export, err, want)
		}
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
