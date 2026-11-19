package e2e

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestEndToEndAMatrixHookFiresOnce: as in Concourse, a hook on an across: step guards the step, so it fires once around the whole matrix — it was copied onto every cell and fired there too.
func TestEndToEndAMatrixHookFiresOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tidied := filepath.Join(dir, "tidied.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - across:
    - var: shard
      values: [a, b]
    task: work
    run: echo {{ .vars.shard }}
    ensure:
      task: tidy
      run: echo tidied >> %s
`, tidied))

	mustRun(t, "run", path, "--job", "build")

	assertLineCount(t, tidied, 1)
}
