package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/testsshd"
)

// The issue's ssh:// test (#206): a bare placed step runs over plain ssh — tar in, sh -c, tar out — against an sshd with no daemon behind it.
func TestSSHWorkerRunsABarePlacedStep(t *testing.T) {
	server := testsshd.New(t)

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
    inputs: [data]
    outputs: [report]
    run: |
      cat data/seed.txt > report/out.txt
      printf '%s' "$STEPS_WORKER" >> report/out.txt

  - task: publish
    inputs: [report]
    run: cp report/out.txt `+published+`
`)

	mustRun(t, path, "--worker", "box="+server.URL)

	if got := readFileString(t, published); got != "seed\nbox" {
		t.Errorf("published = %q, want the input and the worker's tag", got)
	}

	if server.Execs.Load() == 0 {
		t.Error("nothing ran over ssh")
	}

	if remote := placementNamed(t, runPlacements(t, path), "remote"); !strings.HasPrefix(remote.Address, "ssh://") || remote.BytesSent == 0 || remote.BytesReceived == 0 {
		t.Errorf("placement = %+v, want an ssh:// address and the tree both ways", remote)
	}
}

// refusedBeforeAnyStep runs a pipeline whose first, local step leaves a marker; a refusal at placement must come before it.
func refusedBeforeAnyStep(t *testing.T, worker, placed, want string) {
	t.Helper()

	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: first
    run: touch `+marker+`
`+placed)

	err := cli.Run([]string{path, "--worker", "box=" + worker})
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("run: %v, want a refusal saying %q", err, want)
	}

	_, statErr := os.Stat(marker)
	if statErr == nil {
		t.Fatal("a step ran before the placement was refused")
	}
}

func TestSSHWorkerRefusesAnImageBeforeAnyStep(t *testing.T) {
	refusedBeforeAnyStep(t, testsshd.New(t).URL, `
  - task: remote
    tags: [box]
    image: `+dockerE2EImage+`
    run: "true"
`, "runs steps bare and cannot run image:")
}

func TestDockerSSHWorkerRefusesAStepWithNoImageBeforeAnyStep(t *testing.T) {
	refusedBeforeAnyStep(t, "docker+ssh://box", `
  - task: remote
    tags: [box]
    run: "true"
`, "names no image")
}
