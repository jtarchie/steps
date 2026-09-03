package e2e

// End-to-end placement: a tagged step, through the real CLI, onto a real shim.
//
// These use local:, which runs the shim as a child process on this machine
// (see TestMain's dispatch). That is not a stub — it is the whole transport:
// frames, the tree out, the command, the tree back, the exit code. The only
// thing an ssh:// worker adds underneath is a different pipe.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// workerPipeline is a job whose second step runs somewhere else, consuming the
// first step's output and producing one of its own.
func workerPipeline(t *testing.T, dir string) string {
	t.Helper()

	return writePipeline(t, dir, `
jobs:
- name: train
  plan:
  - task: prepare
    outputs: [data]
    run: echo seed > data/seed.txt
  - task: train
    tags: [gpu]
    inputs: [data]
    outputs: [model]
    run: |
      cat data/seed.txt > model/report.txt
      echo "$STEPS_WORKER" >> model/report.txt
  - task: publish
    inputs: [model]
    run: cp model/report.txt `+filepath.Join(dir, "published.txt")+`
`)
}

// TestEndToEndWorkerRoundTripsAStep is the feature, end to end: the step's
// inputs reach the worker, its outputs come back into the local artifact
// store, and a LATER step consumes them without knowing any of it happened.
func TestEndToEndWorkerRoundTripsAStep(t *testing.T) {
	dir := t.TempDir()
	path := workerPipeline(t, dir)

	mustRun(t, path, "--worker", "gpu=local:")

	published := readFileString(t, filepath.Join(dir, "published.txt"))

	if !strings.Contains(published, "seed") {
		t.Errorf("published = %q, want the input the worker consumed", published)
	}

	// The tag reached the command, which is what proves the step ran through a
	// venue rather than here.
	if !strings.Contains(published, "gpu") {
		t.Errorf("published = %q, want STEPS_WORKER to name the tag", published)
	}
}

// TestEndToEndWorkerStepCachesLikeALocalOne is the invariant that let tags:
// stay out of the merkle hash: a placed step is the same work, cached the same
// way. A second run must skip it rather than re-run it, which is only true if
// the tree that crossed the wire digests identically to one that never left.
func TestEndToEndWorkerStepCachesLikeALocalOne(t *testing.T) {
	dir := t.TempDir()
	path := workerPipeline(t, dir)

	mustRun(t, path, "--worker", "gpu=local:")

	first := readFileString(t, filepath.Join(dir, "published.txt"))

	err := os.Remove(filepath.Join(dir, "published.txt"))
	if err != nil {
		t.Fatalf("removing the published file: %v", err)
	}

	// Same pipeline, same inputs: every step is a cache hit, so nothing runs
	// and nothing is republished.
	mustRun(t, path, "--worker", "gpu=local:")

	_, err = os.Stat(filepath.Join(dir, "published.txt"))
	if err == nil {
		t.Fatal("the second run re-executed a step it had already recorded — a placed step is not caching like a local one")
	}

	if first == "" {
		t.Fatal("the first run published nothing")
	}
}

// TestEndToEndUnmappedTagRefusesBeforeRunning pins the promise the DSL makes:
// a step that says it needs a particular machine does not quietly run on this
// one. The refusal has to come before any step executes.
func TestEndToEndUnmappedTagRefusesBeforeRunning(t *testing.T) {
	dir := t.TempDir()
	path := workerPipeline(t, dir)

	err := cli.Run([]string{path})
	if err == nil {
		t.Fatal("a job with an unmapped tag ran anyway")
	}

	if !strings.Contains(err.Error(), "gpu") {
		t.Errorf("error = %v, want it to name the unmapped tag", err)
	}

	// Nothing ran: not even the untagged step before the tagged one.
	assertNoFile(t, filepath.Join(dir, "published.txt"))
}

// TestEndToEndWorkerFailureIsAStepFailure pins the classification across the
// whole stack. A command that ran on a worker and exited nonzero has to reach
// the pipeline as the step failing, so on_failure fires rather than on_error.
func TestEndToEndWorkerFailureIsAStepFailure(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: compile
    tags: [gpu]
    run: exit 3
    on_failure:
      task: note
      run: echo failed > `+filepath.Join(dir, "on-failure.txt")+`
    on_error:
      task: note
      run: echo errored > `+filepath.Join(dir, "on-error.txt")+`
`)

	err := cli.Run([]string{path, "--worker", "gpu=local:"})
	if err == nil {
		t.Fatal("a step whose command exited 3 reported success")
	}

	readFileString(t, filepath.Join(dir, "on-failure.txt"))
	assertNoFile(t, filepath.Join(dir, "on-error.txt"))
}
