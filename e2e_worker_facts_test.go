package main

// What the machine that ran the step was.

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// runPlacements returns the placement rows of the most recent run.
func runPlacements(t *testing.T, pipelinePath string) []store.Placement {
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

	placements, err := st.RunPlacements(context.Background(), runs[0].ID)
	if err != nil {
		t.Fatalf("reading placements: %v", err)
	}

	return placements
}

// TestEndToEndRecordsWhatRanTheStep is the diagnosis half of placement.
//
// The event row already names WHICH machine — a tag and an address. What it
// cannot say is what that machine turned out to BE: the architecture, the
// filesystem the tree landed on, how many bytes had to be pushed there. Those
// are the answers to "it passed on my laptop and failed on the fleet", and
// they are only knowable from the worker's own hello, which nothing outside
// the session could see.
//
// Facts and not a price, deliberately: what an instance-hour costs is not
// something this process can honestly know, and a wrong number in a cost
// column is worse than no column.
func TestEndToEndRecordsWhatRanTheStep(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: here
    outputs: [seed]
    run: head -c 4096 /dev/urandom > seed/blob
  - task: there
    tags: [gpu]
    inputs: [seed]
    run: test -s seed/blob
`)

	err := run([]string{path, "--worker", "gpu=local:"})
	if err != nil {
		t.Fatalf("running: %v", err)
	}

	placements := runPlacements(t, path)

	// Exactly one: the untagged step ran on this machine, and there is no
	// worker to describe. A row for it would claim a placement that never
	// happened.
	if len(placements) != 1 {
		t.Fatalf("recorded %d placements, want 1 (only the tagged step ran on a worker): %+v", len(placements), placements)
	}

	placement := placements[0]

	if placement.StepName != "there" {
		t.Errorf("step_name = %q, want the tagged step", placement.StepName)
	}

	if placement.Tag != "gpu" {
		t.Errorf("tag = %q, want the tag that chose the machine", placement.Tag)
	}

	if placement.JobName != "build" {
		t.Errorf("job_name = %q, want build", placement.JobName)
	}

	assertMachineFacts(t, placement)
}

// assertMachineFacts holds the half of a placement that comes from the
// worker's own hello rather than from this end's bookkeeping.
func assertMachineFacts(t *testing.T, placement store.Placement) {
	t.Helper()

	// From the worker's own hello, not from this process's runtime — the
	// whole point is that a placed step can run somewhere else. A local:
	// worker happens to agree, which is what makes it assertable here.
	if placement.GOOS != runtime.GOOS || placement.GOARCH != runtime.GOARCH {
		t.Errorf("platform = %s/%s, want %s/%s", placement.GOOS, placement.GOARCH, runtime.GOOS, runtime.GOARCH)
	}

	if placement.Workdir == "" {
		t.Error("workdir is empty — the record cannot say where on the machine the tree landed")
	}

	if placement.FSType == "" {
		t.Error("fstype is empty — a tmpfs workdir is the failure this column exists to name")
	}

	// The tree went somewhere, so bytes moved. Zero here would mean the
	// column is wired to something that never counts.
	if placement.BytesSent <= 0 {
		t.Errorf("bytes_sent = %d, want the tree that was pushed to the worker", placement.BytesSent)
	}
}

// TestEndToEndReportsWhereStepsRan is the other half: a row nothing can read
// is not a record. `steps runs --where` is the CLI that answers it.
func TestEndToEndReportsWhereStepsRan(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: seeded-locally
    outputs: [seed]
    run: head -c 4096 /dev/urandom > seed/blob
  - task: there
    tags: [gpu]
    inputs: [seed]
    run: test -s seed/blob
`)

	err := run([]string{path, "--worker", "gpu=local:"})
	if err != nil {
		t.Fatalf("running: %v", err)
	}

	out := captureStdout(t, func() {
		err = run([]string{"runs", path, "--where"})
	})
	if err != nil {
		t.Fatalf("steps runs --where: %v", err)
	}

	for _, want := range []string{"there", "gpu", runtime.GOOS + "/" + runtime.GOARCH} {
		if !strings.Contains(out, want) {
			t.Errorf("steps runs --where did not mention %q:\n%s", want, out)
		}
	}

	// The untagged step ran here; naming it would claim a placement that
	// never happened.
	if strings.Contains(out, "seeded-locally") {
		t.Errorf("steps runs --where named a step that ran locally:\n%s", out)
	}
}

// TestEndToEndWhereNamesOneRunAndSaysWhenNoneWerePlaced covers the two
// answers --where gives beside the table: a specific run, and a run that
// never left this machine.
//
// Two runs, because one cannot tell a --run that is honoured from a --run
// that is ignored in favour of the newest — they are the same run. The
// PLACED one is the older, so a report that quietly takes the latest says
// the opposite of the truth.
//
// The unplaced answer is worth stating out loud: a pipeline that names no
// worker is the ordinary case, and an empty table for it reads as "the
// record is broken" rather than "nothing was placed".
func TestEndToEndWhereNamesOneRunAndSaysWhenNoneWerePlaced(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: placed
  plan:
  - task: on-a-worker
    tags: [gpu]
    run: "true"
- name: unplaced
  plan:
  - task: right-here
    run: "true"
`)

	err := run([]string{"run", path, "--job", "placed", "--worker", "gpu=local:"})
	if err != nil {
		t.Fatalf("running the placed job: %v", err)
	}

	placedRun := latestRunID(t, path)

	err = run([]string{"run", path, "--job", "unplaced"})
	if err != nil {
		t.Fatalf("running the unplaced job: %v", err)
	}

	// No --run: the newest, which is the one that never left this machine.
	out := captureStdout(t, func() {
		err = run([]string{"runs", path, "--where"})
	})
	if err != nil {
		t.Fatalf("steps runs --where: %v", err)
	}

	if !strings.Contains(out, "ran every step on this machine") {
		t.Errorf("steps runs --where on an unplaced run printed:\n%s", out)
	}

	// Named: the older run, which did.
	out = captureStdout(t, func() {
		err = run([]string{"runs", path, "--where", "--run", placedRun})
	})
	if err != nil {
		t.Fatalf("steps runs --where --run: %v", err)
	}

	if !strings.Contains(out, "on-a-worker") {
		t.Errorf("steps runs --where --run %s does not report the run it was asked about:\n%s", placedRun, out)
	}
}

// latestRunID is the newest recorded run of a pipeline.
func latestRunID(t *testing.T, pipelinePath string) string {
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

	return runs[0].ID
}
