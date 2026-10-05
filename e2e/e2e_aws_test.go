package e2e

// A pipeline, on a real EC2 instance, through the real CLI.
//
// The venue package's real-AWS tests prove the transport; this proves the
// product: `steps run --worker` with an aws:// mapping, where a placed step's
// inputs, command and outputs cross real SSM to the instance's docker daemon,
// and a later local step consumes what came back without knowing any of it
// happened.
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

// TestRealAWSPlacedStepRunsInAContainer is the composition nothing else can
// prove: a step placed on a real EC2 instance, running in a container on that
// instance's own docker daemon, reached through a socket forwarded over the
// SSM session.
//
// Every layer here is the real one — the tunnel is SSM, the daemon is the
// worker's, the image is pulled by the worker, and the
// tree lives in that daemon's volumes. A local: worker proves the plumbing; only
// this proves it survives the transport it was designed for.
func TestRealAWSPlacedStepRunsInAContainer(t *testing.T) {
	instance := os.Getenv("STEPS_TEST_AWS_INSTANCE")
	if instance == "" {
		t.Skip("no AWS fixture — run hack/aws-fixture.sh up and export what it prints")
	}

	region := os.Getenv("STEPS_TEST_AWS_REGION")

	worker := "aws://" + instance

	// The instance's region, named on the mapping: on a profile with no default it is the only thing that says where the instance lives.
	if region != "" {
		worker += "?region=" + region
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
    image: public.ecr.aws/docker/library/alpine:3
    inputs: [data]
    outputs: [report]
    run: |
      cat data/seed.txt > report/out.txt
      cat /etc/alpine-release >> report/out.txt
      uname -m >> report/out.txt
      echo "$STEPS_WORKER" >> report/out.txt
  - task: publish
    inputs: [report]
    run: cp report/out.txt `+filepath.Join(dir, "published.txt")+`
`)

	mustRun(t, path, "--worker", "aws="+worker)

	published := readFileString(t, filepath.Join(dir, "published.txt"))

	// seed: the input reached the container. 3.: alpine's own release file,
	// which exists in the image and on neither machine. aarch64: the
	// Graviton worker, not this one. aws: the tag reached the step.
	for _, want := range []string{"seed", "3.", "aarch64", "aws"} {
		if !strings.Contains(published, want) {
			t.Errorf("published = %q, want it to contain %q", published, want)
		}
	}
}
