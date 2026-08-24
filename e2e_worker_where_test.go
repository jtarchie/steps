package main

// Which machine ran the step.

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// stepEvents returns the recorded events of the most recent run.
func stepEvents(t *testing.T, pipelinePath string) []store.RunEventRow {
	t.Helper()

	st, err := store.OpenStore(statePath(pipelinePath, ""), pipelineName(pipelinePath))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	runs, err := st.ListRuns(context.Background(), "", 1)
	if err != nil || len(runs) == 0 {
		t.Fatalf("listing runs: %v (%d found)", err, len(runs))
	}

	rows, err := st.RunEvents(context.Background(), runs[0].ID, 0, 500)
	if err != nil {
		t.Fatalf("reading run events: %v", err)
	}

	return rows
}

// TestEndToEndRecordsWhereAStepRan is the question a red build on a fleet
// cannot currently answer.
//
// Placement is invisible after the fact: nothing in nodes, run_steps or
// run_events says which machine a step ran on, and the merkle hash
// deliberately excludes tags: so it cannot be recovered from the cache either.
// "It passes on my laptop and fails in CI" is the whole class of bug this
// feature introduces, and the record had no column to answer it with.
func TestEndToEndRecordsWhereAStepRan(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: here
    run: "true"
  - task: there
    tags: [gpu]
    run: "true"
`)

	err := run([]string{path, "--worker", "gpu=local:"})
	if err != nil {
		t.Fatalf("running: %v", err)
	}

	placed, local := "", ""

	for _, row := range stepEvents(t, path) {
		if row.Type != "step_finished" {
			continue
		}

		switch row.StepName {
		case "there":
			placed = row.Worker
		case "here":
			local = row.Worker
		}
	}

	if placed == "" {
		t.Fatal("the placed step's record does not say where it ran — a failure on a fleet names no machine")
	}

	if !strings.Contains(placed, "gpu") {
		t.Errorf("worker = %q, want the tag that chose the machine", placed)
	}

	// Absence is the signal: a step with no tags: ran here, and saying so
	// would put a worker on every row of every pipeline that has none.
	if local != "" {
		t.Errorf("an untagged step recorded worker %q, want nothing", local)
	}
}
