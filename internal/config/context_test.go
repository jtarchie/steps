package config

// context: is now only the from: mapping — see contextfrom_test.go for its
// own load-time rules. This file covers the shape of ContextSpec itself: what
// makes it load at all, and where it is rejected outright.

import "testing"

func TestContextRejectsAnythingButAMapping(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    messages:
      - go
    context: write
`)

	wantLoadError(t, path, "must be a {from: ...} mapping")
}

func TestContextRejectsAnUnknownKey(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    messages:
      - go
    context: { write: true }
`)

	wantLoadError(t, path, `unknown key "write"`)
}

func TestContextEnablesNothingIsRejected(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    messages:
      - go
    context: {}
`)

	wantLoadError(t, path, "enables nothing")
}

// TestContextOnHookIsRejected guards the walk gap: validateContextFrom only
// visits job.Plan, so a hook's context: from: would otherwise load clean and
// silently do nothing.
func TestContextOnHookIsRejected(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: critic
  source: { model: lmstudio/qwen }
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: critic
    inputs: []
    messages:
      - judge it
    verdicts: [approve, revise]
  - task: work
    inputs: []
    run: "true"
    on_failure:
      agent: writer
      messages:
        - notify
      context:
        from:
          critic: verdict
`)

	wantLoadError(t, path, "context is not valid on hook steps")
}

func TestContextOnPutIsRejected(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: critic
  source: { model: lmstudio/qwen }
resource_types:
- name: dummy
  config:
    check: "echo '[]'"
    in: "true"
    out: "true"
resources:
- name: results
  type: dummy
  source: {}
jobs:
- name: j
  plan:
  - agent: critic
    inputs: []
    messages:
      - judge it
    verdicts: [approve, revise]
  - put: results
    inputs: []
    context:
      from:
        critic: verdict
`)

	wantLoadError(t, path, "context from: is only valid on agent and task steps")
}
