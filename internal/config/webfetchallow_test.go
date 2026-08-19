package config

import "testing"

// TestWebFetchAllowOnlyBindsOnTheGrant: a step's (or fix's) tools: SELECTS a
// granted tool, and resolveEffectiveTools substitutes the agent's own spec —
// so anything the selection carried is dropped on the way through. An allow:
// written there would read as a fence and bind nothing, which is the one
// failure mode a security control must not have. Refused at load instead.
func TestWebFetchAllowOnlyBindsOnTheGrant(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"step selection": `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools: [web_fetch]
jobs:
- name: j
  plan:
  - agent: reviewer
    inputs: []
    prompt: x
    tools:
    - builtin: web_fetch
      allow: [docs.example]
`,
		"step fix override": `
agents:
- name: fixer
  source: { model: lmstudio/qwen }
  tools: [web_fetch]
tasks:
- name: unit
  run: "true"
jobs:
- name: j
  plan:
  - task: unit
    inputs: []
    fix:
      agent: fixer
      prompt: fix it
      tools:
      - builtin: web_fetch
        allow: [docs.example]
`,
		"task fix override": `
agents:
- name: fixer
  source: { model: lmstudio/qwen }
  tools: [web_fetch]
tasks:
- name: unit
  run: "true"
  fix:
    agent: fixer
    prompt: fix it
    tools:
    - builtin: web_fetch
      allow: [docs.example]
jobs:
- name: j
  plan: [{ task: unit, inputs: [] }]
`,
	}

	for name, pipeline := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			wantLoadError(t, writeConfig(t, pipeline), "allow: binds only where the tool is granted")
		})
	}
}

// TestWebFetchAllowEntryShape: an entry is a bare hostname and nothing else.
// The two paths read the list differently — steps compares it to
// url.Hostname(), the claude CLI compiles it into WebFetch(domain:…) rules —
// so a pattern-shaped entry does not merely fail to match, it can mean
// OPPOSITE things on the two backends. "*" is the worst case: inert on the
// hosted path (deny everything), a documented match-all wildcard on the CLI.
func TestWebFetchAllowEntryShape(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"url":            "https://docs.example",
		"wildcard":       "*",
		"subdomain glob": "*.docs.example",
		"port":           "docs.example:8443",
		"path":           "docs.example/spec",
		"comma-joined":   "a.example,b.example",
		"leading dot":    ".docs.example",
		"empty":          "",
	}

	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			pipeline := `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - builtin: web_fetch
    allow: ["` + entry + `"]
jobs:
- name: j
  plan: [{ agent: reviewer, prompt: x, inputs: [] }]
`

			wantLoadError(t, writeConfig(t, pipeline), "must be a bare hostname")
		})
	}
}

// TestWebFetchAllowAcceptsHostnames pins the other side of the shape rule:
// an apex, a subdomain, and an IPv4 literal all load. Without this the
// validator above could tighten into rejecting everything and no test would
// notice.
func TestWebFetchAllowAcceptsHostnames(t *testing.T) {
	t.Parallel()

	pipeline := `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - builtin: web_fetch
    allow: [specification.website, docs.backerkit.com, 127.0.0.1]
jobs:
- name: j
  plan: [{ agent: reviewer, prompt: x, inputs: [] }]
`

	cfg, err := LoadConfig(writeConfig(t, pipeline))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got := cfg.Agents[0].Tools[0].Allow; len(got) != 3 {
		t.Errorf("allow = %v, want all three entries preserved", got)
	}
}

// TestWebFetchAllowSelectionWithoutAllowStillResolves: selecting the tool by
// bare name from a fenced grant is the supported way to narrow a step, and it
// must keep the AGENT's fence rather than losing it.
func TestWebFetchAllowSelectionWithoutAllowStillResolves(t *testing.T) {
	t.Parallel()

	pipeline := `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  tools:
  - read_file
  - builtin: web_fetch
    allow: [specification.website]
jobs:
- name: j
  plan:
  - agent: reviewer
    inputs: []
    prompt: x
    tools: [web_fetch]
`

	cfg, err := LoadConfig(writeConfig(t, pipeline))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	ri, err := cfg.ResolveAgentInvocation(cfg.Jobs[0].Plan[0])
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	if len(ri.ToolSpecs) != 1 || len(ri.ToolSpecs[0].Allow) != 1 || ri.ToolSpecs[0].Allow[0] != "specification.website" {
		t.Errorf("resolved tools = %+v, want the agent's fenced web_fetch grant", ri.ToolSpecs)
	}
}
