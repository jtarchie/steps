package main

// End-to-end coverage for object fan-out (#42): a from: axis whose recorded
// array holds flat objects, each cell naming the fields it needs.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestEndToEndAcrossFromObjects is the whole feature on one pass: a step
// records structured work items, and each becomes a cell that interpolates the
// fields it cares about — the shape a review pipeline needs, where a finding
// carries a file, a line and a claim rather than an opaque id.
//
// No model: the array is recorded by a shell command into the same context
// store an agent's set_context writes to, which keeps the test about fan-out
// rather than about a scripted conversation.
func TestEndToEndAcrossFromObjects(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "cells.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: verify
  plan:
  - task: scan
    inputs: []
    context: write
    run: |
      mkdir -p context
      printf '[{"id":"SQLI-42","file":"users.py","line":42},{"id":"AUTH-7","file":"api.py","line":7}]' > context/findings
  - across:
    - var: finding
      from: findings
      label: id
    task: verify
    inputs: []
    run: echo "{{ .vars.finding.id }} {{ .vars.finding.file }}:{{ .vars.finding.line }}" >> %[1]s
`, log))

	mustRun(t, path)

	got := strings.TrimSpace(readFileString(t, log))
	want := "SQLI-42 users.py:42\nAUTH-7 api.py:7"

	if got != want {
		t.Errorf("cells rendered:\n%s\nwant:\n%s", got, want)
	}

	// label: gives each cell an identity, which is what makes a matrix
	// addressable in a run's record and by assert:.
	nodes := storeNodes(t, path)
	for _, name := range []string{"verify [finding=SQLI-42]", "verify [finding=AUTH-7]"} {
		assertSucceeded(t, nodes, "task", name)
	}
}

// TestEndToEndAcrossFromObjectsCacheStably pins that an object cell hashes to
// the same thing twice: a rerun of an unchanged pipeline skips every cell.
//
// Not as trivial as it reads. A cell's identity is built from a map, and an
// object item adds a second one — any ordering that leaked into the hash would
// show up here as cells that re-run at random, which is the failure mode that
// makes a cache worse than none.
//
// What this deliberately does NOT test is "changing an unreferenced field
// leaves the cell alone", even though that is true of the cell's own content
// (internal/config's TestAcrossObjectCellsHashLikeTheTextTheyRender pins it).
// It is unobservable from out here: the array can only change by changing the
// step that records it, and that step is the cells' parent, so every cell's
// identity moves with it.
func TestEndToEndAcrossFromObjectsCacheStably(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: verify
  plan:
  - task: scan
    inputs: []
    context: write
    run: |
      mkdir -p context
      printf '[{"id":"A","file":"a.py","line":1},{"id":"B","file":"b.py","line":2}]' > context/findings
  - across:
    - var: finding
      from: findings
      label: id
    task: verify
    inputs: []
    run: echo "{{ .vars.finding.file }}:{{ .vars.finding.line }}" >> %[1]s
`, log))

	mustRun(t, path)
	assertLineCount(t, log, 2)

	mustRun(t, path)
	assertLineCount(t, log, 2)
}

// TestEndToEndAcrossFromObjectsRejectsBadShapes covers what a model can get
// wrong about an array nobody reviewed. Each must fail the step with a message
// naming the key and the shape, rather than fanning out over something the
// cells cannot render.
func TestEndToEndAcrossFromObjectsRejectsBadShapes(t *testing.T) {
	cases := []struct {
		name, recorded, template, wantErr string
	}{
		{
			name:     "mixed shapes",
			recorded: `[{"id":"A"},"plain"]`,
			template: "{{ .vars.finding.id }}",
			wantErr:  "mixes shapes",
		},
		{
			name:     "nested field",
			recorded: `[{"id":"A","where":{"file":"a.py"}}]`,
			template: "{{ .vars.finding.id }}",
			wantErr:  "nested object",
		},
		{
			name:     "object interpolated without a field",
			recorded: `[{"id":"A"}]`,
			template: "{{ .vars.finding }}",
			wantErr:  "name a field",
		},
		{
			name:     "an item missing the named field",
			recorded: `[{"id":"A","file":"a.py"},{"id":"B"}]`,
			template: "{{ .vars.finding.file }}",
			wantErr:  "map has no entry for key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: verify
  plan:
  - task: scan
    inputs: []
    context: write
    run: |
      mkdir -p context
      printf '%[1]s' > context/findings
  - across:
    - var: finding
      from: findings
    task: verify
    inputs: []
    run: echo "%[2]s"
`, tc.recorded, tc.template))

			err := run([]string{path})
			if err == nil {
				t.Fatal("run succeeded; a shape the cells cannot render must fail the step")
			}

			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
