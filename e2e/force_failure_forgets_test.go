package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// A --force rerun that fails must not leave the older success standing for
// the next run to skip on — wherever in the chain the failure is. The
// failure condition is a file the step reads, outside every hash, so all
// three runs plan the same chain.
func TestForcedFailureForgetsTheOlderSuccess(t *testing.T) {
	const tail = `
  - task: c
    inputs: []
    run: echo ran >> %[2]s
`

	cases := map[string]string{
		"last task": `
  - task: a
    inputs: []
    run: echo a
  - task: c
    inputs: []
    run: echo ran >> %[2]s && test ! -f %[1]s
`,
		"middle task": `
  - task: a
    inputs: []
    run: echo a
  - task: b
    inputs: []
    run: test ! -f %[1]s
` + tail,
		"middle task after a get": `
  - get: thing
  - task: b
    inputs: []
    run: test ! -f %[1]s
` + tail,
		"a get fetched in place": `
  - get: thing
  - get: flaky
` + tail,
		"inside do": `
  - task: a
    inputs: []
    run: echo a
  - do:
    - task: b
      inputs: []
      run: test ! -f %[1]s
` + tail,
		"inside in_parallel": `
  - task: a
    inputs: []
    run: echo a
  - in_parallel:
    - task: b
      inputs: []
      run: test ! -f %[1]s
    - task: b2
      inputs: []
      run: echo b2
` + tail,
	}

	for name, plan := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			flag := filepath.Join(dir, "fail")
			counter := filepath.Join(dir, "c-counter.txt")
			path := pipelinePath(t, dir)

			writePipelineFile(t, path, fmt.Sprintf(`
resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: "true"
- name: flaky
  config:
    check: echo '[{"ref":"v1"}]'
    in: test ! -f %[1]s

resources:
- name: thing
  type: dummy
- name: flaky
  type: flaky

jobs:
- name: build
  plan:`+plan, flag, counter))

			mustRun(t, path)
			assertLineCount(t, counter, 1)

			err := os.WriteFile(flag, nil, 0o600)
			if err != nil {
				t.Fatal(err)
			}

			err = cli.Run([]string{"--force", path})
			if err == nil {
				t.Fatal("forced run: want failure, got success")
			}

			err = cli.Run([]string{path})
			if err == nil {
				t.Fatal("rerun after a forced failure: want it to re-run and fail, got success (skipped on the older success)")
			}
		})
	}
}
