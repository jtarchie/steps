package config

import (
	"strings"
	"testing"
)

func sourceSecretsPipeline(source, rest string) string {
	return "agents:\n- name: reviewer\n  source: " + source + "\n" + rest + "jobs:\n- name: review\n  plan:\n  - agent: reviewer\n    messages: [hi]\n"
}

func TestSourceSecretsLoadWhereTheModelClientReadsThem(t *testing.T) {
	t.Parallel()

	for name, pipeline := range map[string]string{
		"named":            sourceSecretsPipeline("{ endpoint: 'http://x/v1/', model: m, api_key_env: K, secrets: { K: ((ci:key)) } }", ""),
		"provider default": sourceSecretsPipeline("{ model: openrouter/x, secrets: { OPENROUTER_API_KEY: ((ci:key)) } }", ""),
		"fallback":         sourceSecretsPipeline("{ model: openrouter/x }", "  fallback:\n  - source: { model: openai/y, api_key_env: K2, secrets: { K2: ((ci:backup)) } }\n"),
		"fix agent":        sourceSecretsPipeline("{ model: openrouter/x, secrets: { OPENROUTER_API_KEY: ((ci:key)) } }", "tasks:\n- name: t\n  run: 'true'\n  fix: reviewer\n"),
		"sub-agent":        sourceSecretsPipeline("{ model: openrouter/x, secrets: { OPENROUTER_API_KEY: ((ci:key)) } }", "- name: lead\n  source: { model: openrouter/y }\n  tools: [{agent: reviewer, description: d}]\n"),
	} {
		cfg, err := Parse([]byte(pipeline), "test", nil)
		if err != nil {
			t.Errorf("%s: Parse = %v, want it loaded", name, err)

			continue
		}

		refs := cfg.SecretRefs()
		if len(refs) != 1 || refs[0].Source != "ci" {
			t.Errorf("%s: SecretRefs = %v, want the source's one reference", name, refs)
		}

		uses := cfg.SecretUses()
		if len(uses) != 1 || uses[0].Job != "review" || uses[0].Resource != "" {
			t.Errorf("%s: SecretUses = %+v, want the reference under the job that reaches the agent", name, uses)
		}
	}
}

func TestSourceSecretsAreRefusedWhereNothingWouldReadThem(t *testing.T) {
	t.Parallel()

	for pipeline, want := range map[string]string{
		sourceSecretsPipeline("{ endpoint: 'http://x/v1/', model: m, secrets: { K: ((ci:key)) } }", ""):                                                        "needs api_key_env",
		sourceSecretsPipeline("{ endpoint: 'http://x/v1/', model: m, api_key_env: OTHER, secrets: { K: ((ci:key)) } }", ""):                                    "OTHER is not a key",
		sourceSecretsPipeline("{ model: openrouter/x, secrets: { K: ((ci:key)) } }", ""):                                                                       "OPENROUTER_API_KEY is not a key",
		sourceSecretsPipeline("{ model: openrouter/x, api_key_env: K, secrets: { K: ((ci:key)), EXTRA: ((ci:more)) } }", ""):                                   "nothing would read EXTRA",
		sourceSecretsPipeline("{ model: openrouter/x, api_key_env: K, secrets: { K: plain-text } }", ""):                                                       "exactly one ((source:name))",
		sourceSecretsPipeline("{ model: '@claude/sonnet', api_key_env: K, secrets: { K: ((ci:key)) } }", ""):                                                   "hooks run",
		sourceSecretsPipeline("{ model: openrouter/x }", "  fallback:\n  - source: { model: '@claude/sonnet', api_key_env: K, secrets: { K: ((ci:key)) } }\n"): "hooks run",
		sourceSecretsPipeline("{ model: openrouter/x }", "  fallback:\n  - source: { model: openai/y, api_key_env: K, secrets: { J: ((ci:key)) } }\n"):         "fallback[0]",
		sourceSecretsPipeline("{ model: openrouter/x, api_key_env: ((ci:key)) }", ""):                                                                          "belongs only in a secrets map",
	} {
		_, err := Parse([]byte(pipeline), "test", nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s\n= %v, want refused naming %q", pipeline, err, want)
		}
	}
}

// An export server has no run, so nothing would resolve the key of an agent it reaches, however deep.
func TestSourceSecretsAreRefusedBehindAnExport(t *testing.T) {
	t.Parallel()

	keyed := "- name: keyed\n  source: { model: openrouter/x, secrets: { OPENROUTER_API_KEY: ((ci:key)) } }\n"
	backup := "- name: keyed\n  source: { model: openrouter/x }\n  fallback:\n  - source: { model: openai/y, secrets: { OPENAI_API_KEY: ((ci:key)) } }\n"
	lead := "- name: lead\n  source: { model: openrouter/y }\n  tools: [{agent: keyed, description: d}]\n"
	jobs := "jobs:\n- name: j\n  plan:\n  - task: t\n    run: 'true'\n"

	for name, pipeline := range map[string]string{
		"direct":   "agents:\n" + keyed + "mcp_exports:\n- {name: a, access: 'true', tools: [{agent: keyed, description: d}]}\n" + jobs,
		"nested":   "agents:\n" + keyed + lead + "mcp_exports:\n- {name: a, access: 'true', tools: [{agent: lead, description: d}]}\n" + jobs,
		"fallback": "agents:\n" + backup + "mcp_exports:\n- {name: a, access: 'true', tools: [{agent: keyed, description: d}]}\n" + jobs,
	} {
		_, err := Parse([]byte(pipeline), "test", nil)
		if err == nil || !strings.Contains(err.Error(), "no run to resolve") {
			t.Errorf("%s: Parse = %v, want the export refused", name, err)
		}
	}

	_, err := Parse([]byte("agents:\n"+keyed+lead+"mcp_exports:\n- {name: a, access: 'true', tools: [{name: t, run: 'echo hi', description: d}]}\n"+jobs), "test", nil)
	if err != nil {
		t.Errorf("an export reaching no keyed agent = %v, want it loaded", err)
	}
}

// A key the daemon resolves is never read from its environment, so its absence there is no problem to report.
func TestCheckAgentCredentialsSkipsASecretBackedKey(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(sourceSecretsPipeline("{ endpoint: 'http://x/v1/', model: m, api_key_env: STEPS_TEST_NOBODY_SETS_THIS, secrets: { STEPS_TEST_NOBODY_SETS_THIS: ((ci:key)) } }", "")), "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	problems := cfg.CheckEnvironment()
	if len(problems) != 0 {
		t.Errorf("CheckEnvironment = %v, want nothing: the key comes from a secret source", problems)
	}
}
