package e2e

// A worker that dies mid-step is infrastructure, and attempts: is the author
// saying try again — so the retry has to reach a fresh shim, not the dead
// pipe the last attempt left behind.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestEndToEndWorkerRedialsAfterTheShimDies kills the shim on the first
// attempt and requires the second to succeed. The runner is shared across
// attempts by design (see TestEndToEndWorkerRetryKeepsScratch), so without a
// redial the retry writes into a pipe nobody holds and fails for a reason the
// pipeline author cannot see or fix.
func TestEndToEndWorkerRedialsAfterTheShimDies(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")

	// kill -9 leaves the shim no chance to remove its scratch, so the test does, by a step name no other test uses since every e2e test shares this pid.
	t.Cleanup(func() {
		left, _ := filepath.Glob(filepath.Join(os.TempDir(), "steps-shim", fmt.Sprintf("*-shim-dies-%d-*", os.Getpid())))
		for _, scratch := range left {
			_ = os.RemoveAll(scratch)
		}
	})

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: shim-dies
    tags: [gpu]
    attempts: 2
    outputs: [out]
    run: |
      if [ ! -f `+marker+` ]; then
        touch `+marker+`
        kill -9 $PPID
        exit 1
      fi
      echo revived > out/f.txt
  - task: publish
    inputs: [out]
    run: cp out/f.txt `+filepath.Join(dir, "published.txt")+`
`)

	err := cli.Run([]string{path, "--worker", "gpu=local:"})
	if err != nil {
		t.Fatalf("the retried task failed: %v\n\nthe second attempt did not reach a fresh shim after the first one died", err)
	}

	published := readFileString(t, filepath.Join(dir, "published.txt"))
	if !strings.Contains(published, "revived") {
		t.Errorf("published = %q, want the output the second attempt produced", published)
	}
}
