package main

// A placed step that runs in a container: `tags:` and `image:` together.
//
// The two have always been refused together, on the grounds that a worker
// runs a step's commands directly. That is a statement about the transport,
// not about what a pipeline author wants — a remote machine with a toolchain
// baked into its AMI is a worse answer than a remote machine with a docker
// daemon, and every other venue property (the tree goes out, the outputs come
// back, placement stays outside the hash) is unchanged by which of the two
// runs the command.
//
// This is the whole feature as a user sees it, so it is the first test.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlacedStepRunsInAContainer places a step on a local: worker AND gives
// it an image, then proves from inside the step which of the two ran it.
//
// alpine's /etc/alpine-release exists only in the image: a host-executed step
// on this machine cannot read it, so its contents are the proof that the
// command ran in the container rather than beside it. The workspace assertion
// is the other half — the tree that was sent has to be what the container
// sees, or the step ran somewhere with none of its inputs.
func TestPlacedStepRunsInAContainer(t *testing.T) {
	requireDockerE2E(t)

	// The step's tree is bind-mounted into the container by the DAEMON, so it
	// has to live somewhere the daemon can see. On macOS the daemon runs in a
	// VM that shares the home directory and not /var/folders, where TMPDIR
	// points — and docker answers an unshared mount by silently mounting an
	// EMPTY directory, so the step succeeds and produces nothing. This is the
	// same constraint docs/infra.md documents for local image: steps; it
	// reaches placed ones for exactly the same reason.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to root a daemon-visible workspace in")
	}

	shared, err := os.MkdirTemp(home, ".steps-e2e-*")
	if err != nil {
		t.Skipf("cannot create a daemon-visible temp dir under %s: %v", home, err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(shared) })
	t.Setenv("TMPDIR", shared)

	dir := t.TempDir()
	published := filepath.Join(dir, "published.txt")

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: prepare
    outputs: [data]
    run: echo seed > data/seed.txt

  - task: remote
    tags: [box]
    image: `+dockerE2EImage+`
    inputs: [data]
    outputs: [report]
    run: |
      cat data/seed.txt > report/out.txt
      cat /etc/alpine-release >> report/out.txt

  - task: publish
    inputs: [report]
    run: cp report/out.txt `+published+`
`)

	mustRun(t, path, "--worker", "box=local:")

	got := readFileString(t, published)

	// The input crossed to the worker, and the command that read it was
	// running inside the image.
	if !strings.Contains(got, "seed") {
		t.Errorf("published = %q, want the step's input to have reached the container", got)
	}

	if !strings.Contains(got, "3.") {
		t.Errorf("published = %q, want alpine's release file — the command ran on the host, not in the image", got)
	}
}
