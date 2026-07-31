package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// steps validate reports everything wrong with a pipeline in one pass, and —
// the point of the command — touches nothing while doing it: no state store,
// no workspace, no containers, no step executed.
func TestValidateReportsAllErrorsAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: j
  plan:
  - task: t
    run: touch `+filepath.Join(dir, "ran.txt")+`
    trigger: true
  - agent: ghost
    prompt: x
`)

	err := run([]string{"validate", path})
	if err == nil {
		t.Fatal("expected validate to fail on a broken pipeline")
	}

	for _, want := range []string{
		"trigger is only valid on get steps",
		`no agent named "ghost"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q\n  is missing %q", err.Error(), want)
		}
	}

	// Nothing ran and nothing was persisted: validating is a read.
	_, statErr := os.Stat(filepath.Join(dir, "ran.txt"))
	if statErr == nil {
		t.Error("validate executed a step")
	}

	_, statErr = os.Stat(filepath.Join(dir, ".steps"))
	if statErr == nil {
		t.Error("validate created a state store")
	}
}

func TestValidateAcceptsAValidPipeline(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: j
  plan:
  - task: greet
    run: echo hello
`)

	err := run([]string{"validate", path})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// Artifact flow is checked for every job, not just the one a run would have
// selected — the whole file is the unit being validated.
func TestValidateChecksArtifactFlowInEveryJob(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
workspace:
  strategy: copy
jobs:
- name: fine
  plan:
  - task: a
    run: "true"
    outputs: [built]
  - task: b
    run: "true"
    inputs: [built]
- name: broken
  plan:
  - task: c
    run: "true"
    inputs: [never-produced]
`)

	err := run([]string{"validate", path})
	if err == nil {
		t.Fatal("expected validate to reject an input nothing produces")
	}

	if !strings.Contains(err.Error(), "never-produced") {
		t.Errorf("error = %q, want it to name the undeclared artifact", err.Error())
	}
}

// Every shipped example validates, so `steps validate examples/<x>.yml` is a
// working first command for a reader who just copied one.
func TestValidateExamples(t *testing.T) {
	matches, err := filepath.Glob("examples/*.yml")
	if err != nil {
		t.Fatal(err)
	}

	if len(matches) == 0 {
		t.Fatal("no examples found")
	}

	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
			err := run([]string{"validate", path})
			if err != nil {
				t.Errorf("validate %s: %v", path, err)
			}
		})
	}
}
