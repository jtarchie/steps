package main

import (
	"os"
	"strings"
	"testing"
)

// steps plan answers "what would this run do?" without doing it. The planner
// always computed this and acted on it immediately, so the only way to find
// out what a run would skip was to start one.
func TestPlanReportsRunVersusSkip(t *testing.T) {
	dir := t.TempDir()
	marker := dir + "/ran.txt"
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: compile
    run: touch `+marker+`
`)

	// Nothing has run: the step is pending.
	out := captureStdout(t, func() {
		err := run([]string{"plan", path})
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
	})

	if !strings.Contains(out, "run") || !strings.Contains(out, "compile") {
		t.Errorf("plan output %q, want it to show compile as a step that would run", out)
	}

	// Planning must not execute anything.
	_, err := os.Stat(marker)
	if err == nil {
		t.Fatal("plan executed the step")
	}

	// After a real run, the same plan reports it cached.
	err = run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	out = captureStdout(t, func() {
		planErr := run([]string{"plan", path})
		if planErr != nil {
			t.Fatalf("plan: %v", planErr)
		}
	})

	if !strings.Contains(out, "skip") || !strings.Contains(out, "cached") {
		t.Errorf("plan output %q, want compile reported as cached", out)
	}
}

// Planning records nothing: no node, no job_run. A preview that left history
// behind would be lying about being a read.
func TestPlanRecordsNothing(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: compile
    run: "true"
`)

	err := run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	before := captureStdout(t, func() {
		runsErr := run([]string{"runs", path, "--steps"})
		if runsErr != nil {
			t.Fatalf("runs: %v", runsErr)
		}
	})

	captureStdout(t, func() {
		planErr := run([]string{"plan", path})
		if planErr != nil {
			t.Fatalf("plan: %v", planErr)
		}
	})

	after := captureStdout(t, func() {
		runsErr := run([]string{"runs", path, "--steps"})
		if runsErr != nil {
			t.Fatalf("runs: %v", runsErr)
		}
	})

	if before != after {
		t.Errorf("planning changed recorded history:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A step with a when: guard cannot be cached, and the plan says so in the
// same words the run itself uses rather than leaving the reader to wonder.
func TestPlanNamesWhyAStepCannotBeCached(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: check
    run: "true"
    when: test -f /nonexistent
`)

	out := captureStdout(t, func() {
		err := run([]string{"plan", path})
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
	})

	if !strings.Contains(out, "when: guard") {
		t.Errorf("plan output %q, want it to name the when: guard as the reason", out)
	}
}
