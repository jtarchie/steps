package config

import (
	"strings"
	"testing"
)

const rulesBase = `
mcp_servers:
- name: github
  endpoint: https://example.test/mcp
jobs:
- name: build
  plan:
  - task: say
    run: echo hi
agents:
- name: judge
  source: { model: openrouter/x, api_key_env: K }
- name: cli-judge
  source: { model: '@claude/sonnet' }
`

func TestRulesRefuseWhatCannotWork(t *testing.T) {
	t.Parallel()

	for rules, want := range map[string]string{
		"rules:\n- {name: a}":                                            "checks nothing",
		"rules:\n- {name: 'a b', when: 'true'}":                          "name:",
		"rules:\n- {name: a, when: 'true'}\n- {name: a, accept: 'true'}": "declared twice",
		"rules:\n- {name: a, judged_by: nobody}":                         "nobody",
		"rules:\n- {name: a, judged_by: cli-judge}":                      "cli source",
		"rules:\n- {name: a, when: 'true'}\nmcp_exports:\n- {name: e, access: 'true', rules: [nope], tools: [{mcp: github}]}":                                  "nope",
		"rules:\n- {name: a, when: 'true'}\nmcp_exports:\n- {name: e, access: 'true', tools: [{mcp: github, rules: [nope]}]}":                                  "nope",
		"- name: x\n  source: { model: openrouter/x, api_key_env: K }\n  tools:\n  - {mcp: github, tool: t, rules: [nope]}\nrules:\n- {name: a, when: 'true'}": "nope",
	} {
		_, err := Parse([]byte(rulesBase+rules), "test", nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s\n= %v, want refused naming %q", rules, err, want)
		}
	}
}

func TestRulesLoadWhereTheyAreAttached(t *testing.T) {
	t.Parallel()

	pipeline := rulesBase + `
rules:
- {name: acme-only, when: 'args.owner in who.Organizations'}
- {name: no-secrets, accept: 'not (result.text matches "key")'}
- {name: sanity, judged_by: judge}
mcp_exports:
- name: support
  access: 'true'
  rules: [no-secrets]
  tools:
  - {mcp: github, tool: search, rules: [acme-only, sanity]}
`
	cfg, err := Parse([]byte(pipeline), "test", nil)
	if err != nil || len(cfg.Rules) != 3 || cfg.MCPExports[0].Rules[0] != "no-secrets" || cfg.MCPExports[0].Tools[0].Rules[1] != "sanity" {
		t.Fatalf("Parse = %+v, %v; want the rules loaded and attached", cfg, err)
	}
}

func TestARuleOnAStepSelectionIsRefusedBecauseItWouldNotBind(t *testing.T) {
	t.Parallel()

	pipeline := strings.Replace(rulesBase, "  - task: say\n    run: echo hi", "  - agent: worker\n    messages: [hi]\n    tools:\n    - {mcp: github, tool: t, rules: [r]}", 1) + `- name: worker
  source: { model: openrouter/x, api_key_env: K }
  tools:
  - {mcp: github, tool: t}
rules:
- {name: r, when: 'true'}
`
	_, err := Parse([]byte(pipeline), "test", nil)
	if err == nil || !strings.Contains(err.Error(), "binds only where the tool is granted") {
		t.Errorf("a rules: on a step's selection = %v, want refused", err)
	}
}

func TestAJudgeIsNotItselfJudged(t *testing.T) {
	t.Parallel()

	pipeline := rulesBase + `- name: warden
  source: { model: openrouter/x, api_key_env: K }
  tools:
  - {mcp: github, tool: t, rules: [sanity]}
rules:
- {name: sanity, judged_by: warden}
`
	_, err := Parse([]byte(pipeline), "test", nil)
	if err == nil || !strings.Contains(err.Error(), "a judge answers, it is not judged") {
		t.Errorf("a judge held to its own rule = %v, want refused", err)
	}
}
