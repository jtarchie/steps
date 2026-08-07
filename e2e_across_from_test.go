package main

// End-to-end coverage for runtime fan-out (#39): an across: axis whose values
// come from what an earlier step recorded, rather than from the pipeline text.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestEndToEndAcrossFromRuntimeArray is the whole feature on one pass: a task
// records a JSON array, and the matrix runs one cell per item — with the item
// interpolated into the cell exactly as a static values: entry would be.
//
// No model: the array is recorded by a shell command, which is the same
// context store an agent's set_context writes to. That keeps the test about
// fan-out rather than about a scripted conversation.
func TestEndToEndAcrossFromRuntimeArray(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "cells.log")
	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: audit
  plan:
  - task: scan
    inputs: []
    context: write
    run: |
      mkdir -p context
      printf '["alpha","beta","gamma"]' > context/findings
  - across:
    - var: finding
      from: findings
    task: "investigate-{{ .vars.finding }}"
    inputs: []
    run: echo "{{ .vars.finding }}" >> %[1]s
`, log))

	mustRun(t, path)

	// One cell per item, in the array's order.
	got := strings.Fields(readFileString(t, log))
	want := []string{"alpha", "beta", "gamma"}

	if len(got) != len(want) {
		t.Fatalf("cells ran = %v, want one per recorded item %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cell %d ran for %q, want %q (cells follow the recorded array's order)", i, got[i], want[i])
		}
	}

	// Each cell is its own node, named by the value it ran for — which is what
	// makes a matrix readable in a run's record and addressable by assert:.
	nodes := storeNodes(t, path)
	for _, name := range []string{"investigate-alpha", "investigate-beta", "investigate-gamma"} {
		assertSucceeded(t, nodes, "task", name)
	}
}

// TestEndToEndAcrossFromRejectsBadSources covers what a model can get wrong
// about an array nobody reviewed: the wrong shape, and an unrecorded key. Both
// must fail the step with a message naming the key, not expand to zero cells
// and look like a matrix that ran.
func TestEndToEndAcrossFromRejectsBadSources(t *testing.T) {
	cases := []struct {
		name, record, wantErr string
	}{
		{
			name:    "not an array",
			record:  `printf 'just a string' > context/findings`,
			wantErr: "must hold a JSON array of strings",
		},
		{
			name:    "empty array",
			record:  `printf '[]' > context/findings`,
			wantErr: "holds an empty array",
		},
		{
			name:    "key never recorded",
			record:  `printf 'x' > context/something_else`,
			wantErr: "which nothing in this run recorded",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: audit
  plan:
  - task: scan
    inputs: []
    context: write
    run: |
      mkdir -p context
      %[1]s
  - across:
    - var: finding
      from: findings
    task: "investigate-{{ .vars.finding }}"
    inputs: []
    run: "true"
`, tc.record))

			err := run([]string{path})
			if err == nil {
				t.Fatal("run succeeded; a bad runtime source must fail the step")
			}

			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
