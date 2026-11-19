package e2e

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestEndToEndAFailingOnSuccessRecordsTheStepFailed: a failing on_success fails its step, and the step's node must say so — it was left "succeeded", which is what the matrix cache asks a node before skipping it.
func TestEndToEndAFailingOnSuccessRecordsTheStepFailed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ran := filepath.Join(dir, "ran.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: work
    run: echo ran >> %s
    on_success:
      task: notify
      run: "false"
`, ran))

	for range 2 {
		err := stepsRun(t, []string{"run", path, "--job", "build"})
		if err == nil {
			t.Fatal("a failing on_success did not fail the job")
		}
	}

	assertLineCount(t, ran, 2)

	for _, node := range storeNodes(t, path) {
		if node.Resource == "work" && node.Status != "failed" {
			t.Errorf("work's node is %q, want failed: its on_success failed it", node.Status)
		}
	}
}
