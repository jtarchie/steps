package config

import (
	"slices"
	"strings"
	"testing"
)

// Every row of #263's table, as the place each would be named.
const fencePipeline = `
mcp_servers:
- name: notes
  command: notes-mcp

mcp_exports:
- name: bare
  access: "true"
  tools: [{name: hello, description: say hi, run: echo hi}]
- name: boxed
  access: "true"
  image: alpine:3
  tools: [{name: hello, description: say hi, run: echo hi}]
- name: root
  access: "true"
  image: alpine:3
  privileged: true
  tools: [{name: hello, description: say hi, run: echo hi}]

resource_types:
- name: boxed
  image: alpine:3
  config: {check: "true", in: "true"}
- name: rooted
  image: alpine:3
  privileged: true
  config: {check: "true", in: "true"}
- name: fenced
  sandbox: true
  config: {check: "true", in: "true"}
- name: counters
  config:
    expr:
      check: '[{"n": "1"}]'
      in: '{"n.txt": version.n}'

resources:
- name: repo
  type: git
  source: {uri: https://example.com/acme/app.git}
- name: boxed-thing
  type: boxed
  source: {}
- name: rooted-thing
  type: rooted
  source: {}
- name: fenced-thing
  type: fenced
  source: {}
- name: tick
  type: counters
  source: {}

agents:
- name: reader
  source: {model: openai/gpt-4o-mini}
- name: boxed-reader
  source: {model: openai/gpt-4o-mini}
  image: alpine:3
- name: rooted-reader
  source: {model: openai/gpt-4o-mini}
  image: alpine:3
  privileged: true
- name: hooked
  source: {model: "@claude/sonnet"}
  image: alpine:3
  settings: project
- name: chat
  source: {model: openai/gpt-4o-mini}
  tools: [web_fetch]

jobs:
- name: build
  plan:
  - get: boxed-thing
  - get: fenced-thing
  - get: tick
  - task: loose
    run: make
  - task: boxed
    image: alpine:3
    run: make
  - task: rooted
    image: alpine:3
    privileged: true
    run: make
  - task: fenced
    sandbox: true
    run: make
  - agent: reader
  - agent: boxed-reader
  - agent: rooted-reader
  - agent: hooked
  - agent: chat
  - load_var: n
    file: tick/n.txt
  ensure:
    task: notify
    run: echo done
`

func TestUnfencedNamesEveryCommandNothingFences(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(fencePipeline), "fence", nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	where := func(sandboxHere bool) []string {
		found := cfg.Unfenced(sandboxHere)
		names := make([]string, 0, len(found))

		for _, host := range found {
			names = append(names, host.Where)
		}

		return names
	}

	always := []string{
		`mcp_servers "notes"`, `mcp_exports "bare"`, `mcp_exports "root"`,
		`resource "repo"`, `resource "rooted-thing"`,
		`task "loose"`, `task "rooted"`, `task "notify"`,
		`agent "reader"`, `agent "rooted-reader"`, `agent "hooked"`,
	}
	never := []string{`mcp_exports "boxed"`, `resource "boxed-thing"`, `resource "tick"`, `task "boxed"`, `agent "boxed-reader"`, `agent "chat"`}
	sandboxed := []string{`resource "fenced-thing"`, `task "fenced"`}

	for _, tc := range []struct {
		name        string
		sandboxHere bool
		want, not   []string
	}{
		{"darwin", true, always, append(slices.Clone(never), sandboxed...)},
		{"elsewhere", false, append(slices.Clone(always), sandboxed...), never},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := where(tc.sandboxHere)

			for _, want := range tc.want {
				if !slices.ContainsFunc(got, func(w string) bool { return strings.HasPrefix(w, want) }) {
					t.Errorf("%s is not listed as unfenced: %q", want, got)
				}
			}

			for _, not := range tc.not {
				if slices.ContainsFunc(got, func(w string) bool { return strings.HasPrefix(w, not) }) {
					t.Errorf("%s is listed as unfenced, and it is fenced or runs no command: %q", not, got)
				}
			}
		})
	}
}

// One entry per place: a sandboxed CLI agent with settings: project is one hole, not two lines.
func TestUnfencedNamesAPlaceOnce(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(`
agents:
- name: hooked
  source: {model: "@claude/sonnet"}
  sandbox: true
  settings: project

jobs:
- name: build
  plan:
  - agent: hooked
`), "fence", nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	for _, sandboxHere := range []bool{true, false} {
		if got := cfg.Unfenced(sandboxHere); len(got) != 1 {
			t.Errorf("sandboxHere=%v: %d entries, want the one agent: %+v", sandboxHere, len(got), got)
		}
	}
}
