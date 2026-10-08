package config

import "testing"

func TestMemoryLoadErrors(t *testing.T) {
	t.Parallel()

	agents := `
agents:
- name: bot
  source: {model: m, endpoint: "http://127.0.0.1:1/v1/"}
- name: saver
  description: saves facts
  source: {model: m, endpoint: "http://127.0.0.1:1/v1/"}
  tools: [remember, forget]
`

	for name, tc := range map[string]struct{ pipeline, want string }{
		"on a task": {`
jobs:
- name: j
  plan:
  - task: t
    run: "true"
    memory: {scope_from: who/user}
`, "memory is only valid on agent steps"},
		"on a hook": {agents + `
jobs:
- name: j
  plan:
  - task: t
    run: "true"
    on_success:
      agent: bot
      memory: {scope_from: who/user}
`, "memory is not valid on hook steps"},
		"a memory grant on a hook": {agents + `
jobs:
- name: j
  plan:
  - task: t
    run: "true"
  ensure:
    agent: saver
`, "remember and forget are not valid on a hook step"},
		"no file": {agents + `
jobs:
- name: j
  plan:
  - agent: bot
    inputs: [who]
    memory: {scope_from: who}
`, "must be <input>/<file>"},
		"an escaping file": {agents + `
jobs:
- name: j
  plan:
  - agent: bot
    inputs: [who]
    memory: {scope_from: who/../secrets}
`, "without . or .. segments"},
		"a dotted file": {agents + `
jobs:
- name: j
  plan:
  - agent: bot
    inputs: [who]
    memory: {scope_from: who/./user}
`, "without . or .. segments"},
		"a tasks: entry whose fix saves": {agents + `
tasks:
- name: t
  run: "false"
  fix: saver
jobs:
- name: j
  plan:
  - task: t
`, "used as a fix: agent"},
		"a sub-agent that saves": {agents + `
- name: lead
  source: {model: m, endpoint: "http://127.0.0.1:1/v1/"}
  tools: [{agent: saver}]
jobs:
- name: j
  plan:
  - agent: lead
    messages: [hi]
`, `agent "saver": remember and forget are not valid on an agent used as a sub-agent`},
		"a fix agent that saves": {agents + `
jobs:
- name: j
  plan:
  - task: t
    run: "false"
    fix: saver
`, "used as a fix: agent"},
		"negative entries": {`
defaults:
  memory_entries: -1
jobs:
- name: j
  plan:
  - task: t
    run: "true"
`, "memory_entries must not be negative"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			wantLoadError(t, writeConfig(t, tc.pipeline), tc.want)
		})
	}
}

// TestMemoryWithoutAWriterLoads: a step may be handed its memory and not
// write it — the chat agent of a pair whose second agent does the saving.
func TestMemoryWithoutAWriterLoads(t *testing.T) {
	t.Parallel()

	cfg := loadOK(t, `
defaults:
  memory_entries: 0
agents:
- name: bot
  source: {model: m, endpoint: "http://127.0.0.1:1/v1/"}
jobs:
- name: j
  plan:
  - task: who
    outputs: [who]
    run: echo U1 > who/user
  - agent: bot
    inputs: [who]
    memory: {scope_from: who/user}
    messages: [hi]
`)

	if got := cfg.MemoryEntriesLimit(); got != 0 {
		t.Errorf("MemoryEntriesLimit = %d, want 0 (no limit)", got)
	}

	if got := (&Config{}).MemoryEntriesLimit(); got != DefaultMemoryEntries {
		t.Errorf("unset MemoryEntriesLimit = %d, want %d", got, DefaultMemoryEntries)
	}
}
