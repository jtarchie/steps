package main

// timeout: on a job — a wall-clock ceiling on the whole run, the other unit of
// the ceiling budget: already provides.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestJobTimeoutStopsBeforeTheNextStep is the contract: the step that was
// running finishes and keeps its work, and no further step is started.
//
// The first task sleeps past the deadline. If the deadline cancelled the
// context instead of being checked between steps, that task would be killed
// mid-sleep and never write its marker — which is exactly what this asserts
// did not happen.
func TestJobTimeoutStopsBeforeTheNextStep(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: slow
  timeout: 1s
  plan:
  - task: overruns
    inputs: []
    run: |
      sleep 1.5
      echo overruns >> %[1]s
  - task: never-starts
    inputs: []
    run: echo never-starts >> %[1]s
`, log))

	err := run([]string{path})
	if err == nil {
		t.Fatal("the job succeeded; a passed deadline must fail it")
	}

	if !strings.Contains(err.Error(), "exceeded its timeout") {
		t.Errorf("error = %v, want it to name the timeout", err)
	}

	got := readFileString(t, log)

	// The overrunning step ran to completion — it was not interrupted.
	if !strings.Contains(got, "overruns") {
		t.Errorf("the running step was cut off; log:\n%s", got)
	}

	// And the next one never started.
	if strings.Contains(got, "never-starts") {
		t.Errorf("a step started after the deadline had passed; log:\n%s", got)
	}
}

// TestJobTimeoutLeavesAFastJobAlone pins that the deadline is a ceiling and not
// a schedule: a job that finishes inside it is untouched.
func TestJobTimeoutLeavesAFastJobAlone(t *testing.T) {
	dir := t.TempDir()

	path := writePipeline(t, dir, `
jobs:
- name: quick
  timeout: 1h
  plan:
  - task: a
    inputs: []
    run: "true"
  - task: b
    inputs: []
    run: "true"
  assert:
    execution: [a, b]
    outcome: succeeded
`)

	mustRun(t, path)
}

// TestJobTimeoutReachesJobHooks covers the classification. A deadline breach is
// a job-level FAILURE — an operator set a bound and the run crossed it, the
// same class as exceeding max_visits — so on_failure fires. That is where a
// "this took too long" notification belongs, and it only works if the breach
// is failed rather than errored or aborted.
func TestJobTimeoutReachesJobHooks(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "hook.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: slow
  timeout: 1s
  plan:
  - task: overruns
    inputs: []
    run: sleep 1.5
  - task: never-starts
    inputs: []
    run: "true"
  on_failure:
    task: notify
    run: echo on_failure >> %[1]s
  ensure:
    task: cleanup
    run: echo ensure >> %[1]s
`, log))

	err := run([]string{path})
	if err == nil {
		t.Fatal("the job succeeded; a passed deadline must fail it")
	}

	got := readFileString(t, log)
	for _, want := range []string{"on_failure", "ensure"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s hook did not fire on a deadline breach; log:\n%s", want, got)
		}
	}
}

// TestJobTimeoutRejectsWhatCannotBeMeant covers the two spellings a load must
// refuse rather than guess at.
func TestJobTimeoutRejectsWhatCannotBeMeant(t *testing.T) {
	cases := []struct{ name, timeout, wantErr string }{
		{name: "not a duration", timeout: "soon", wantErr: "timeout"},
		{name: "zero", timeout: "0s", wantErr: "positive duration"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: j
  timeout: %q
  plan:
  - task: a
    inputs: []
    run: "true"
`, tc.timeout))

			err := run([]string{"validate", path})
			if err == nil {
				t.Fatalf("timeout: %q loaded", tc.timeout)
			}

			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestJobTimeoutBoundsAFanOut is the regression for the defect the pipeline in
// examples/pr-review.yml found in this very feature, reviewing the commit that
// added it.
//
// The deadline was checked only at the top of the plan walk, and a whole
// across: block is ONE iteration of that loop — so a matrix that ran long was
// never revisited, and a job timeout could not bound the runtime fan-out it
// was built for. Three one-second cells ran to completion against a
// one-second deadline.
//
// Now the matrix stops admitting cells, and the plan walk still fails the job.
func TestJobTimeoutBoundsAFanOut(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "cells.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: fan
  timeout: 1s
  plan:
  - across:
    - var: cell
      values: [a, b, c]
    task: "work-{{ .vars.cell }}"
    inputs: []
    run: |
      sleep 1
      echo {{ .vars.cell }} >> %[1]s
`, log))

	err := run([]string{path})
	if err == nil {
		t.Fatal("the job succeeded; a passed deadline must fail it")
	}

	// The first cell runs (nothing has overrun yet) and takes the deadline
	// past its limit; the rest are never started. Three would mean the matrix
	// ignored the deadline entirely, which is the defect.
	ran := strings.Fields(readFileString(t, log))
	if len(ran) >= 3 {
		t.Errorf("every cell ran despite the deadline: %v", ran)
	}
}
