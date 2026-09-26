package e2e

// tags: on a job and on a block, inherited by the steps inside.

import (
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestEndToEndJobTagsPlaceEveryStep: a job's tags: place an untagged step and
// the job's own hook, a do: block's tags: override it for the steps inside,
// and the block itself — which runs nothing — records no placement.
func TestEndToEndJobTagsPlaceEveryStep(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  tags: [gpu]
  plan:
  - task: a
    run: "true"
  - tags: [disk]
    do:
    - task: b
      run: "true"
  ensure:
    task: tidy
    run: "true"
`)

	err := cli.Run([]string{path, "--worker", "gpu=local:", "--worker", "disk=local:"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got := map[string]string{}
	for _, placement := range runPlacements(t, path) {
		got[placement.StepName] = placement.Tag
	}

	want := map[string]string{"a": "gpu", "b": "disk", "tidy": "gpu"}
	if len(got) != len(want) {
		t.Fatalf("placements = %v, want %v", got, want)
	}

	for step, tag := range want {
		if got[step] != tag {
			t.Errorf("step %q placed on %q, want %q (all: %v)", step, got[step], tag, got)
		}
	}
}

// TestEndToEndAStepsHookGoesWhereTheStepWent: a step's untagged hook inherits
// what the step resolved to, as in Concourse — the tagged step's on_failure:
// is placed on the step's worker, not run here.
func TestEndToEndAStepsHookGoesWhereTheStepWent(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: work
    tags: [gpu]
    run: "false"
    on_failure:
      task: tell-someone
      run: "true"
`)

	err := cli.Run([]string{path, "--worker", "gpu=local:"})
	if err == nil {
		t.Fatal("the pipeline was supposed to fail so its on_failure hook would run")
	}

	got := map[string]string{}
	for _, placement := range runPlacements(t, path) {
		got[placement.StepName] = placement.Tag
	}

	if len(got) != 2 || got["work"] != "gpu" || got["tell-someone"] != "gpu" {
		t.Fatalf("placements = %v, want the step and its hook both on gpu", got)
	}
}
