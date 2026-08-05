package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestAcrossCachesPerCell is the headline advantage over Concourse, which
// re-runs the entire matrix on any change: each cell is its own merkle node,
// so editing one value re-runs only the cells that value appears in.
func TestAcrossCachesPerCell(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran.log")

	pipeline := func(second string) string {
		return fmt.Sprintf(`
jobs:
- name: build
  plan:
  - across:
    - var: suite
      values: [unit, %s]
    task: check
    inputs: []
    run: echo {{ .vars.suite }} >> %s
`, second, log)
	}

	path := writePipeline(t, dir, pipeline("integration"))
	mustRun(t, path)
	assertLineCount(t, log, 2)

	// Re-running unchanged re-runs nothing: both cells are cached.
	mustRun(t, path)
	assertLineCount(t, log, 2)

	// Changing the SECOND axis value must re-run only that cell. If cells
	// shared one node — or if the matrix were hashed as a unit — the unit cell
	// would run again too and this would be 4.
	path = writePipeline(t, dir, pipeline("e2e"))
	mustRun(t, path)
	assertLineCount(t, log, 3)

	if got := readFileString(t, log); strings.Count(got, "unit") != 1 {
		t.Errorf("the unchanged cell re-ran:\n%s", got)
	}
}

// TestAcrossRejectsAnEmptyAxis covers the shape that would expand to nothing
// at all — a matrix that silently runs zero steps is the worst possible
// reading of a typo.
func TestAcrossRejectsAnEmptyAxis(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - across:
    - var: suite
      values: []
    task: check
    inputs: []
    run: "true"
`)

	err := run([]string{"validate", path})
	if err == nil {
		t.Fatal("an axis with no values loaded")
	}

	if !strings.Contains(err.Error(), "no values") {
		t.Errorf("error does not explain the empty axis: %v", err)
	}
}

// TestAcrossRejectsADuplicateVar covers the axis that would silently shadow
// another.
func TestAcrossRejectsADuplicateVar(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - across:
    - var: suite
      values: [a]
    - var: suite
      values: [b]
    task: check
    inputs: []
    run: "true"
`)

	err := run([]string{"validate", path})
	if err == nil {
		t.Fatal("a duplicated axis name loaded")
	}

	if !strings.Contains(err.Error(), "twice") {
		t.Errorf("error does not explain the shadowing: %v", err)
	}
}

// TestAcrossRejectsAnUnknownVar verifies a typo in an interpolation is a load
// error rather than an empty string substituted into a command.
func TestAcrossRejectsAnUnknownVar(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - across:
    - var: suite
      values: [unit]
    task: check
    inputs: []
    run: echo {{ .vars.suit }}
`)

	err := run([]string{"validate", path})
	if err == nil {
		t.Fatal("a misspelled variable loaded")
	}
}
