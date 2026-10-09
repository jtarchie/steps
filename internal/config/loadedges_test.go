package config

import (
	"fmt"
	"strings"
	"testing"
)

// Each case is the input that tells a load rule apart from its off-by-one or inverted twin.
func TestLoadEdges(t *testing.T) {
	t.Parallel()

	const bot = `
agents:
- name: bot
  source: { model: lmstudio/qwen }
`

	oauth := func(port int) string {
		return fmt.Sprintf(`
mcp_servers:
- name: slack
  endpoint: https://mcp.slack.com/mcp
  auth: { type: oauth, client_id: "123.456", callback_port: %d }
jobs: [{ name: j, plan: [] }]
`, port)
	}

	tests := map[string]struct {
		pipeline string
		want     string
	}{
		"credentials in a fallback endpoint": {`
agents:
- name: bot
  source: { model: lmstudio/qwen }
  fallback:
  - source: { model: m, endpoint: "https://user:pass@backup.example.com/v1/" }
jobs: [{ name: j, plan: [] }]
`, "fallback[0].source.endpoint must not embed credentials"},
		"lowest unprivileged callback_port": {oauth(1024), ""},
		"highest callback_port":             {oauth(65535), ""},
		"a fix tool naming an unknown mcp server": {bot + `
tasks:
- name: t
  run: "true"
  fix: { agent: bot, tools: [{ mcp: nowhere }] }
jobs: [{ name: j, plan: [] }]
`, `task "t" fix: mcp tool:`},
		"a misspelled key on a post resource's source": {`
resources:
- name: comment
  type: github-pr-comment
  source: { repo: acme/app, repoo: acme/app }
jobs: [{ name: j, plan: [] }]
`, "repoo"},
		"an http github endpoint": {`
resources:
- name: comment
  type: github-pr-comment
  source: { repo: acme/app, endpoint: "http://ghe.internal/api/v3" }
jobs: [{ name: j, plan: [] }]
`, ""},
		"a non-http github endpoint": {`
resources:
- name: comment
  type: github-pr-comment
  source: { repo: acme/app, endpoint: "ftp://ghe.internal/api/v3" }
jobs: [{ name: j, plan: [] }]
`, "source.endpoint must be an http(s) URL"},
		"context from an unknown step at verdict level": {fromAgents + `
jobs:
- name: j
  plan:
  - agent: critic
    inputs: []
    messages: [judge it]
    verdicts: [approve, revise]
  - agent: writer
    inputs: []
    messages: [write it]
    context: { from: { critc: verdict } }
`, `names "critc", which is not a step in this job (did you mean "critic"?)`},
		"a memory grant on an ask_user responder": {`
agents:
- name: saver
  source: { model: lmstudio/qwen }
  tools: [remember]
- name: bot
  source: { model: lmstudio/qwen }
  tools: [{ builtin: ask_user, answered_by: saver }]
jobs: [{ name: j, plan: [] }]
`, "used as an ask_user responder"},
		"a duplicate output on a tasks: entry": {`
tasks:
- name: t
  run: "true"
  outputs: [out, out]
jobs: [{ name: j, plan: [] }]
`, `task "t": duplicate output "out"`},
		"an unknown ensemble member_errors": {bot + `
jobs:
- name: j
  plan:
  - ensemble:
      verdicts: [approve, reject]
      decide: majority
      member_errors: skip
      agents:
      - { agent: bot, inputs: [], messages: [a] }
      - { agent: bot, inputs: [], messages: [b] }
`, `ensemble.member_errors "skip" is not valid`},
		"an error inside a try:": {`
jobs:
- name: j
  plan:
  - try: { approval: { timeout: 1h } }
`, "(try): approval.message is required"},
		"a sub-agent tool on a step's fix:": {bot + `
jobs:
- name: j
  plan:
  - task: t
    run: "true"
    fix: { agent: bot, tools: [{ agent: bot, description: d }] }
`, "a fix agent may not use sub-agent tools"},
		"a builtin tool on a task's fix:": {bot + `
tasks:
- name: t
  run: "true"
  fix: { agent: bot, tools: [read_file] }
jobs: [{ name: j, plan: [] }]
`, ""},
		"a cli agent as a step's inline fix:": {`
agents:
- name: fixer
  source: { model: "@claude/sonnet" }
jobs:
- name: j
  plan:
  - task: t
    run: "true"
    fix: { agent: fixer }
`, "cannot run as a fix agent"},
		"max_questions 0 on an agent": {`
agents:
- name: bot
  source: { model: lmstudio/qwen }
  max_questions: 0
jobs: [{ name: j, plan: [] }]
`, ""},
		"an agent timeout that does not parse": {`
agents:
- name: bot
  source: { model: lmstudio/qwen }
  timeout: soon
jobs: [{ name: j, plan: [] }]
`, `agent "bot":`},
		"max_turns 0 on an agent step": {bot + `
jobs:
- name: j
  plan:
  - { agent: bot, inputs: [], messages: [x], max_turns: 0 }
`, ""},
		"eight context_paths on a matrix cell": {bot + `
jobs:
- name: j
  plan:
  - agent: bot
    inputs: [src]
    messages: [x]
    across: [{ var: n, values: ["1"] }]
    context_paths: [src/a, src/b, src/c, src/d, src/e, src/f, src/g, src/h]
`, ""},
		"a collecting matrix through a plain try:": {`
jobs:
- name: j
  plan:
  - across: [{ var: n, values: ["1"] }]
    try: { task: work, inputs: [], run: "true", outputs: [findings] }
`, ""},
		"a matrix cell with an output-less hook": {`
jobs:
- name: j
  plan:
  - task: work
    inputs: []
    run: "true"
    across: [{ var: n, values: ["1"] }]
    on_failure: { task: note, inputs: [], run: "true" }
`, ""},
		"a zero approval timeout": {`
jobs:
- name: j
  plan:
  - approval: { message: ship, timeout: 0s }
`, "is not a positive duration"},
		"a fetch uri that only starts with a colon": {`
resources:
- name: repo
  type: git
  source: { uri: ":repo", branch: main, fetch: true }
jobs: [{ name: j, plan: [] }]
`, "needs source.uri to be an absolute path"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadConfig(writeConfig(t, test.pipeline))

			switch {
			case test.want == "" && err != nil:
				t.Fatalf("LoadConfig: %v", err)
			case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
				t.Fatalf("LoadConfig error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}

// maxSubAgentDepth is "a chain of at most this many agents": eight load, nine do not.
func TestSubAgentDepthBoundary(t *testing.T) {
	t.Parallel()

	chain := func(n int) string {
		var b strings.Builder

		b.WriteString("agents:\n")

		for i := range n {
			fmt.Fprintf(&b, "- name: a%d\n  source: { model: lmstudio/qwen }\n", i)

			if i < n-1 {
				fmt.Fprintf(&b, "  tools: [{ agent: a%d, description: d }]\n", i+1)
			}
		}

		b.WriteString("jobs:\n- name: j\n  plan: [{ agent: a0, messages: [x], inputs: [] }]\n")

		return b.String()
	}

	_, err := LoadConfig(writeConfig(t, chain(maxSubAgentDepth)))
	if err != nil {
		t.Fatalf("a chain of %d agents: %v", maxSubAgentDepth, err)
	}

	wantLoadError(t, writeConfig(t, chain(maxSubAgentDepth+1)), "nesting depth exceeded")
}

func TestClosestPrefersTheNearestThenTheFirst(t *testing.T) {
	t.Parallel()

	if got := closest("abcd", []string{"abce", "abxx"}); got != "abce" {
		t.Errorf("closest = %q, want the nearer candidate", got)
	}

	if got := closest("abcd", []string{"abce", "abcf"}); got != "abce" {
		t.Errorf("closest = %q, want the first of two equally near candidates", got)
	}
}
