package e2e

import (
	"strings"
	"testing"
)

// An empty from_file: list is a green run that did nothing, and the run says which file was empty — otherwise it reads exactly like a matrix whose cells all passed.
func TestEndToEndAcrossOverAnEmptyListSaysWhichFileWasEmpty(t *testing.T) {
	dir := t.TempDir()

	path := writePipeline(t, dir, `
jobs:
- name: fan
  plan:
  - task: scan
    inputs: []
    outputs: [findings]
    run: mkdir -p findings && printf '[]' > findings/items.json
  - across:
    - var: item
      from_file: findings/items.json
    task: "investigate-{{ .vars.item }}"
    inputs: [findings]
    run: "true"
`)

	out := captureStdout(t, func() { mustRun(t, path) })

	if want := "across: 0 cells (findings/items.json is empty); nothing to run"; !strings.Contains(out, want) {
		t.Errorf("output does not say the list was empty; want %q in:\n%s", want, out)
	}
}
