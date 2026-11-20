package config

import (
	"strings"
	"testing"
)

// Every kind fence: warn lists names the entity an allowlist matches and the reason the pipeline gave, read from where image: or sandbox: would go.
func TestUnfencedNamesTheEntityAndItsReason(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(`
mcp_servers:
- name: notes
  command: notes-mcp
  unfenced: server reason
- name: plain
  command: plain-mcp

mcp_exports:
- name: bare
  access: "true"
  unfenced: export reason
  tools: [{name: hello, description: say hi, run: echo hi}]
- name: helpers
  access: "true"
  tools: [{agent: reader, description: d}]

resource_types:
- name: git
  unfenced: git reason
- name: rooted
  image: alpine:3
  privileged: true
  unfenced: rooted reason
  config: {check: "true", in: "true"}

resources:
- name: repo
  type: git
  source: {uri: https://example.com/acme/app.git}
- name: rooted-thing
  type: rooted
  source: {}

tasks:
- name: entry
  unfenced: entry reason
  run: make

agents:
- name: reader
  source: {model: openai/gpt-4o-mini}
  unfenced: reader reason
- name: other
  source: {model: openai/gpt-4o-mini}

jobs:
- name: build
  plan:
  - get: rooted-thing
  - task: entry
  - task: inline
    unfenced: inline reason
    run: make
  - task: rooted
    image: alpine:3
    privileged: true
    unfenced: privileged reason
    run: make
  - agent: reader
  - agent: other
    unfenced: step agent reason
  - do:
    - task: entry
    unfenced: guard reason
    when: test -d .
  - load_var: n
    file: rooted-thing/x
    when: test -d .
`), "fence", nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	for _, want := range []struct{ where, entity, reason string }{
		{`mcp_servers "notes"`, "mcp_server notes", "server reason"},
		{`mcp_servers "plain"`, "mcp_server plain", ""},
		{`mcp_exports "bare"`, "mcp_export bare", "export reason"},
		{`mcp_exports "helpers" agent "reader"`, "agent reader", "reader reason"},
		{`resource "repo"`, "resource_type git", "git reason"},
		{`resource "rooted-thing"`, "resource_type rooted", "rooted reason"},
		{`task "entry" in job "build" step 1`, "task entry", "entry reason"},
		{`task "inline"`, "task inline", "inline reason"},
		{`task "rooted"`, "task rooted", "privileged reason"},
		{`agent "reader" in`, "agent reader", "reader reason"},
		{`agent "other" in`, "agent other", "step agent reason"},
		{`when: guard of job "build" step 6`, "guard build", "guard reason"},
		{`when: guard of job "build" step 7`, "guard build", ""},
	} {
		found := false

		for _, host := range cfg.Unfenced(false) {
			if !strings.HasPrefix(host.Where, want.where) {
				continue
			}

			found = true

			if host.Entity != want.entity || host.Reason != want.reason {
				t.Errorf("%s: entity %q reason %q, want %q and %q", host.Where, host.Entity, host.Reason, want.entity, want.reason)
			}
		}

		if !found {
			t.Errorf("%s is not listed: %+v", want.where, cfg.Unfenced(false))
		}
	}

	// unfenced: alone on a built-in's name keeps the built-in, as image: does.
	rt, err := cfg.FindResourceType("git")
	if err != nil || rt.Config.Check == "" {
		t.Errorf("resource_types: git with only unfenced: replaced the built-in: %+v %v", rt, err)
	}
}

// A step's unfenced: is its own say, as its sandbox: is.
func TestUnfencedReasonIsTheStepsOverTheEntrys(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(`
tasks:
- name: entry
  unfenced: entry reason
  run: make
jobs:
- name: build
  plan:
  - task: entry
    unfenced: step reason
`), "fence", nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if got := cfg.Unfenced(false); len(got) != 1 || got[0].Reason != "step reason" {
		t.Errorf("want the step's reason over its entry's: %+v", got)
	}
}

func TestUnfencedIsRefusedWhereItSaysNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, pipeline, want string }{
		{"blank on an entry", "tasks:\n- name: t\n  unfenced: \" \"\n  run: make\njobs:\n- name: b\n  plan:\n  - task: t\n", "needs a reason"},
		{"blank on a step", "jobs:\n- name: b\n  plan:\n  - task: t\n    unfenced: \"\"\n    run: make\n", "needs a reason"},
		{"blank on a server", "mcp_servers:\n- name: s\n  command: s\n  unfenced: \"\"\njobs:\n- name: b\n  plan:\n  - task: t\n    run: make\n", "needs a reason"},
		{"unguarded load_var", "jobs:\n- name: b\n  plan:\n  - task: t\n    run: echo 1 > n\n    outputs: [n]\n  - load_var: n\n    file: n/n\n    unfenced: why\n", "runs none"},
		{"a put", "resource_types:\n- name: shell\n  config: {check: \"true\", in: \"true\", out: \"true\"}\nresources:\n- name: r\n  type: shell\n  source: {}\njobs:\n- name: b\n  plan:\n  - put: r\n    unfenced: why\n    when: test -d .\n", "resource_types: entry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse([]byte(tc.pipeline), "fence", nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one saying %q", err, tc.want)
			}
		})
	}
}
