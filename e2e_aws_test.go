package main

// A pipeline, on a real EC2 instance, through the real CLI.
//
// The venue package's real-AWS tests prove the transport; this proves the
// product: `steps run --worker` with an aws:// mapping and an --artifact-store,
// where a placed step's inputs, command and outputs cross real SSM and real
// S3, and a later local step consumes what came back without knowing any of
// it happened.
//
// Opt-in, same fixture as the venue tests:
//
//	hack/aws-fixture.sh up   # export what it prints
//	go test . -run TestRealAWS -v

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealAWSPipelineStepRunsOnAnInstance is the whole feature end to end.
func TestRealAWSPipelineStepRunsOnAnInstance(t *testing.T) {
	instance := os.Getenv("STEPS_TEST_AWS_INSTANCE")
	bucket := os.Getenv("STEPS_TEST_AWS_BUCKET")
	binary := os.Getenv("STEPS_TEST_AWS_BINARY")

	if instance == "" || bucket == "" || binary == "" {
		t.Skip("no AWS fixture — run hack/aws-fixture.sh up and export what it prints")
	}

	store := "s3://" + bucket + "/steps-test"
	if region := os.Getenv("STEPS_TEST_AWS_REGION"); region != "" {
		store += "?region=" + region
	}

	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: prepare
    outputs: [data]
    run: echo seed > data/seed.txt
  - task: remote
    tags: [aws]
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

	mustRun(t, path,
		"--worker", "aws="+"aws://"+instance+"?binary="+binary,
		"--artifact-store", store)

	published := readFileString(t, filepath.Join(dir, "published.txt"))

	for _, want := range []string{"seed", "aarch64", "aws"} {
		if !strings.Contains(published, want) {
			t.Errorf("published = %q, want it to contain %q", published, want)
		}
	}
}
