package e2e

// `steps validate --require-image`: a pipeline that must run nothing on the
// host is refused by naming every place that would, and --allow-host names
// the deliberate exceptions.

import (
	"strings"
	"testing"
)

const hostPipeline = `
mcp_servers:
- name: notes
  command: notes-mcp

resource_types:
- name: boxed
  image: alpine:3.20
  config:
    check: echo '[{"ref":"1"}]'
    in: "true"

resources:
- name: repo
  type: git
  source:
    uri: https://example.com/acme/app.git
- name: boxed-thing
  type: boxed
  source: {}
- name: pr
  type: github-prs
  source:
    repo: acme/app
    checkout: git
- name: pr-tree
  type: github-prs
  source:
    repo: acme/app

tasks:
- name: fenced
  sandbox: true
  run: echo hi

agents:
- name: reader
  source: {model: openai/gpt-4o-mini}
- name: boxed-reader
  source: {model: openai/gpt-4o-mini}
  image: alpine:3.20
- name: talker
  source: {model: openai/gpt-4o-mini}
  tools: [{mcp: notes}]
- name: lead
  source: {model: openai/gpt-4o-mini}
  tools: [{agent: reader, description: reads}]

jobs:
- name: build
  plan:
  - get: boxed-thing
  - get: pr-tree
  - task: compile
    run: make
  - task: boxed-compile
    image: alpine:3.20
    run: make
  - task: fenced
  - agent: reader
  - agent: boxed-reader
  - agent: talker
  - agent: lead
  on_failure:
    task: notify
    run: echo failed
`

// TestValidateRequireImageNamesEveryHostCommand: each place that would run on the host is named with why, and nothing that runs in an image or in steps' own process is.
func TestValidateRequireImageNamesEveryHostCommand(t *testing.T) {
	t.Parallel()

	path := writePipeline(t, t.TempDir(), hostPipeline)

	mustRun(t, "validate", "--syntax-only", path)

	printed, err := runAndCapture(t, "validate", "--syntax-only", "--require-image", path)
	if err == nil {
		t.Fatalf("validate --require-image passed; printed %s", printed)
	}

	for _, want := range []string{
		`resource "repo"`, `type "git" runs check/in/out on the host`,
		`resource "pr"`, "checkout: git runs the git binary on the host",
		`mcp_servers "notes"`,
		`task "compile"`, `run: runs on the host`,
		`agent "reader"`, "read_file",
		`agent "lead"`, "sub-agent reader",
		`notify`,
		`task "fenced"`, "sandbox: fences it, and it is still the host",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}

	for _, unwanted := range []string{`"boxed-thing"`, `"pr-tree"`, "boxed-compile", "boxed-reader", `agent "talker"`} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("error names %s, which runs in an image or in steps itself:\n%v", unwanted, err)
		}
	}
}

// TestValidateRequireImageAllowHost: --allow-host accepts the places it names and no others, and a name that matches nothing is a typo, refused.
func TestValidateRequireImageAllowHost(t *testing.T) {
	t.Parallel()

	path := writePipeline(t, t.TempDir(), hostPipeline)

	mustRun(t, "validate", "--syntax-only", "--require-image", "--allow-host=repo,pr,notes,compile,fenced,reader,lead,notify", path)

	err := stepsRun(t, []string{"validate", "--syntax-only", "--require-image", "--allow-host=repo,pr,notes,compile,fenced,reader,lead", path})
	if err == nil || !strings.Contains(err.Error(), "notify") || strings.Contains(err.Error(), `resource "repo"`) {
		t.Errorf("err = %v, want only the hook task notify named", err)
	}

	err = stepsRun(t, []string{"validate", "--syntax-only", "--require-image", "--allow-host=repo,pr,notes,compile,fenced,reader,lead,notify,typo", path})
	if err == nil || !strings.Contains(err.Error(), "--allow-host=typo: nothing") {
		t.Errorf("err = %v, want the allowlist entry that names nothing refused", err)
	}

	err = stepsRun(t, []string{"validate", "--syntax-only", "--allow-host=repo", path})
	if err == nil || !strings.Contains(err.Error(), "--require-image") {
		t.Errorf("err = %v, want --allow-host without --require-image refused", err)
	}
}
