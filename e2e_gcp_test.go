package main

// A pipeline, on a real Compute Engine instance, through the real CLI.
//
// The venue package's real-GCP tests prove the transport; this proves the
// product: `steps run --worker` with a gcp:// mapping, where a placed step's
// inputs, command and outputs cross the real IAP relay — with no artifact
// store, which a gcp:// worker does not need.
//
// Opt-in, same fixture as the venue tests:
//
//	hack/gcp-fixture.sh up   # export what it prints
//	go test . -run TestRealGCP -v

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealGCPPipelineStepRunsOnAnInstance is the whole feature end to end.
func TestRealGCPPipelineStepRunsOnAnInstance(t *testing.T) {
	project := os.Getenv("STEPS_TEST_GCP_PROJECT")
	zone := os.Getenv("STEPS_TEST_GCP_ZONE")
	instance := os.Getenv("STEPS_TEST_GCP_INSTANCE")
	binary := os.Getenv("STEPS_TEST_GCP_BINARY")

	if project == "" || zone == "" || instance == "" || binary == "" {
		t.Skip("no GCP fixture — run hack/gcp-fixture.sh up and export what it prints")
	}

	worker := "gcp://" + instance + "/var/tmp/steps?project=" + project + "&zone=" + zone + "&binary=" + binary

	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: prepare
    outputs: [data]
    run: echo seed > data/seed.txt
  - task: remote
    tags: [gcp]
    inputs: [data]
    outputs: [model]
    run: |
      cat data/seed.txt > model/report.txt
      uname -m >> model/report.txt
      echo "$STEPS_WORKER" >> model/report.txt
  - task: publish
    inputs: [model]
    run: cp model/report.txt `+filepath.Join(dir, "published.txt")+`
`)

	mustRun(t, path, "--worker", "gcp="+worker)

	published := readFileString(t, filepath.Join(dir, "published.txt"))

	// seed: the input crossed the tunnel out. x86_64: the fixture worker, not
	// (necessarily) this machine. gcp: STEPS_WORKER named the tag there.
	for _, want := range []string{"seed", "x86_64", "gcp"} {
		if !strings.Contains(published, want) {
			t.Errorf("published = %q, want it to contain %q", published, want)
		}
	}
}
