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
    run: echo ((tag))`, "((tag))"},
		"a race yields only what every branch captured": {`
  - race:
      steps:
      - do:
        - load_var: tag
          file: meta/v
          inputs: [meta]
        - load_var: only_one
          file: meta/v
          inputs: [meta]
      - load_var: tag
        file: meta/v
        inputs: [meta]
  - task: after
    run: echo ((tag)) ((only_one))`, "((only_one))"},
		"a race's common capture is available": {`
  - race:
      steps:
      - load_var: tag
        file: meta/v
        inputs: [meta]
      - load_var: tag
        file: meta/v
        inputs: [meta]
  - task: after
    run: echo ((tag))`, ""},
		"on_success sees the step's own capture": {`
  - do:
    - load_var: tag
      file: meta/v
      inputs: [meta]
    on_success:
      task: tell
      run: echo ((tag))`, ""},
		"on_failure does not": {`
  - do:
    - load_var: tag
      file: meta/v
      inputs: [meta]
    on_failure:
      task: tell
      run: echo ((tag))`, "((tag))"},
		"a job's on_success sees the plan's captures": {`
  - load_var: tag
    file: meta/v
    inputs: [meta]
  on_success:
    task: tell
    run: echo ((tag))`, ""},
		"a job's ensure does not": {`
  - load_var: tag
    file: meta/v
    inputs: [meta]
  ensure:
    task: tell
    run: echo ((tag))`, "((tag))"},
		"a hook sees what preceded its step": {`
  - load_var: tag
    file: meta/v
    inputs: [meta]
  - task: work
    run: "true"
    on_failure:
      task: tell
      run: echo ((tag))`, ""},
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
