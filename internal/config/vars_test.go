package config

import (
	"testing"
)

const varsPick = `
jobs:
- name: build
  plan:
  - task: pick
    outputs: [meta]
    run: "true"
`

// TestVarFlowThroughNestedSteps pins where a capture is guaranteed to have
// happened, which is the only place a ((name)) may count on it.
func TestVarFlowThroughNestedSteps(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		plan string
		want string // "" means it loads
	}{
		"a try body's capture does not escape it": {`
  - try:
      load_var: tag
      file: meta/v
      inputs: [meta]
  - task: after
    run: echo ((.:tag))`, "((.:tag))"},
		"a guarded capture does not escape its guard": {`
  - load_var: tag
    file: meta/v
    inputs: [meta]
    when: test -s meta/v
  - task: after
    when: test -s meta/v
    run: echo ((.:tag))`, "((.:tag))"},
		"but reaches the guarded step's own on_success, which a skip never fires": {`
  - do:
    - load_var: tag
      file: meta/v
      inputs: [meta]
    when: test -s meta/v
    on_success:
      task: tell
      run: echo ((.:tag))`, ""},
		"a guarded do: carries the capture to its consumers": {`
  - do:
    - load_var: tag
      file: meta/v
      inputs: [meta]
    - task: after
      run: echo ((.:tag))
    when: test -s meta/v`, ""},
		"a race yields no branch's capture, since any one may have won": {`
  - race:
      steps:
      - load_var: tag
        file: meta/v
        inputs: [meta]
      - load_var: other
        file: meta/v
        inputs: [meta]
  - task: after
    run: echo ((.:tag))`, "((.:tag))"},
		"two race branches cannot capture one name, as a loser may write it last": {`
  - race:
      steps:
      - load_var: tag
        file: meta/v
        inputs: [meta]
      - load_var: tag
        file: meta/v
        inputs: [meta]`, `load_var "tag" is already the name of an earlier load_var:`},
		"in_parallel branches cannot capture one name": {`
  - in_parallel:
    - load_var: tag
      file: meta/v
      inputs: [meta]
    - load_var: tag
      file: meta/v
      inputs: [meta]`, `load_var "tag" is already`},
		"a capture in an across: cell does not escape it": {`
  - do:
    - load_var: tag
      file: meta/v
      inputs: [meta]
    - task: inside
      run: echo ((.:tag))
    across:
    - var: cell
      values: [a, b]
  - task: after
    run: echo ((.:tag))`, `step 2 (line 18): run references ((.:tag)), which no load_var: or get: earlier`},
		"an across: cell may capture a name the plan already has, as its own": {`
  - load_var: tag
    file: meta/v
    inputs: [meta]
  - do:
    - load_var: tag
      file: meta/v
      inputs: [meta]
    across:
    - var: cell
      values: [a, b]
  - task: after
    run: echo ((.:tag))`, ""},
		"the old spelling names the new one": {`
  - load_var: tag
    file: meta/v
    inputs: [meta]
  - task: after
    run: echo ((tag.version))`, "((.:tag.version))"},
		"a malformed reference is refused": {`
  - load_var: tag
    file: meta/v
    inputs: [meta]
  - task: after
    run: echo ((.:tag..x))`, `"((.:tag..x))"`},
		"a put's facts are refused, naming why": {`
  - put: pick
  - task: after
    run: echo ((.:pick.url))`, "a put's facts are not available"},
		"format: needs a load_var": {`
  - task: after
    run: "true"
    format: json`, "format: without load_var:"},
		"reveal: needs a load_var": {`
  - task: after
    run: "true"
    reveal: true`, "reveal: without load_var:"},
		"an unknown format is refused": {`
  - load_var: tag
    file: meta/v
    inputs: [meta]
    format: toml`, `format "toml" is not one of json, yaml, yml, trim or raw`},
		"on_success sees the step's own capture": {`
  - do:
    - load_var: tag
      file: meta/v
      inputs: [meta]
    on_success:
      task: tell
      run: echo ((.:tag))`, ""},
		"on_failure does not": {`
  - do:
    - load_var: tag
      file: meta/v
      inputs: [meta]
    on_failure:
      task: tell
      run: echo ((.:tag))`, "((.:tag))"},
		"a job's on_success sees the plan's captures": {`
  - load_var: tag
    file: meta/v
    inputs: [meta]
  on_success:
    task: tell
    run: echo ((.:tag))`, ""},
		"a job's ensure does not": {`
  - load_var: tag
    file: meta/v
    inputs: [meta]
  ensure:
    task: tell
    run: echo ((.:tag))`, "((.:tag))"},
		"a hook sees what preceded its step": {`
  - load_var: tag
    file: meta/v
    inputs: [meta]
  - task: work
    run: "true"
    on_failure:
      task: tell
      run: echo ((.:tag))`, ""},
		"a nested load_var is checked for a file": {`
  - do:
    - load_var: tag`, "needs a file"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, varsPick+c.plan+"\n")
			if c.want != "" {
				wantLoadError(t, path, c.want)

				return
			}

			_, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
		})
	}
}

const varsGet = `
resource_types:
- name: echoes
  config:
    check: |
      echo '[{"ref": "v1"}]'
    in: "true"
resources:
- name: pr
  type: echoes
jobs:
- name: build
  plan:
  - get: pr
`

// TestAGetsFactsFlowLikeACapture: a get's name is a ((.:name)) root, from the get on.
func TestAGetsFactsFlowLikeACapture(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		plan string
		want string
	}{
		"a later step reads a fact": {`
  - task: after
    run: echo ((.:pr.url))`, ""},
		"the old spelling names the new one": {`
  - task: after
    run: echo ((pr.url))`, "((.:pr.url))"},
		"a load_var cannot take a get's name": {`
  - task: write
    outputs: [meta]
    run: "true"
  - load_var: pr
    file: meta/v
    inputs: [meta]`, `load_var "pr" is already the name of a get`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, varsGet+c.plan+"\n")
			if c.want != "" {
				wantLoadError(t, path, c.want)

				return
			}

			_, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
		})
	}
}
