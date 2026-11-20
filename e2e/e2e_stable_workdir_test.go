package e2e

// A host step's working directory is stable when free (#259 2d): the same path every run, so a tool keyed on the source directory (Go's build cache) stays warm, unless another run of the step holds it.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestAHostStepRunsAtTheSamePathEveryRun: two runs of one host task see one working directory, under the root, and the second sees none of what the first left there.
func TestAHostStepRunsAtTheSamePathEveryRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wd, seen := filepath.Join(dir, "wd.log"), filepath.Join(dir, "seen.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: make
    outputs: [src]
    run: echo code > src/main.txt
  - task: compile
    volatile: true
    inputs: [src]
    run: |
      pwd -P >> %[1]s
      ls -A | tr '\n' ' ' >> %[2]s
      echo >> %[2]s
      touch leftover src/leftover
`, wd, seen))
	root := wsRoot(path)

	mustRun(t, "run", path, "--job", "build", "--workspace-root", root)
	mustRun(t, "run", path, "--job", "build", "--workspace-root", root, "--force")

	paths := strings.Fields(readFileString(t, wd))
	if len(paths) != 2 || paths[0] != paths[1] {
		t.Fatalf("the task ran at %v, want one path both times", paths)
	}

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(paths[0], filepath.Join(realRoot, "steps-work")+string(filepath.Separator)) {
		t.Errorf("the task ran at %s, want a stable path under %s", paths[0], filepath.Join(realRoot, "steps-work"))
	}

	if got := readFileString(t, seen); got != "src \nsrc \n" {
		t.Errorf("the task saw %q, want only its input both times: a stable path must never carry one run's files into the next", got)
	}
}

// TestAHostAgentWorksAtTheSamePathEveryRun: an agent's tools run in its step directory on this machine, so they get the stable path a host task does.
func TestAHostAgentWorksAtTheSamePathEveryRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wd := filepath.Join(dir, "wd.log")
	shell := map[string]any{"command": "pwd -P >> " + wd}

	fake := newFakeLLM(t,
		callsTool("run_shell", shell), says("done"),
		callsTool("run_shell", shell), says("done"),
	)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: builder
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]

jobs:
- name: build
  plan:
  - agent: builder
    volatile: true
    inputs: []
    messages: ["build it"]
`, fake.URL))
	root := wsRoot(path)

	mustRun(t, "run", path, "--job", "build", "--workspace-root", root)
	mustRun(t, "run", path, "--job", "build", "--workspace-root", root, "--force")

	paths := strings.Fields(readFileString(t, wd))
	if len(paths) != 2 || paths[0] != paths[1] || !strings.Contains(paths[0], string(filepath.Separator)+"steps-work"+string(filepath.Separator)) {
		t.Errorf("the agent's tools ran at %v, want one stable path both times", paths)
	}
}

// TestOverlappingRunsOfAHostStepTakeDifferentPaths: two cells of one step running at once cannot share a directory, so one takes the stable path and the other a throwaway one, as caches: does.
func TestOverlappingRunsOfAHostStepTakeDifferentPaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wd := filepath.Join(dir, "wd.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - across:
    - var: cell
      values: [a, b]
    max_in_flight: 2
    task: compile
    volatile: true
    run: |
      pwd -P >> %[1]s
      touch %[2]s/started-{{ .vars.cell }}
      for i in $(seq 1 200); do
        [ -f %[2]s/started-a ] && [ -f %[2]s/started-b ] && exit 0
        sleep 0.05
      done
      echo "the cells never overlapped" >&2
      exit 1
`, wd, dir))
	root := wsRoot(path)

	mustRun(t, "run", path, "--job", "build", "--workspace-root", root)

	paths := strings.Fields(readFileString(t, wd))
	if len(paths) != 2 || paths[0] == paths[1] {
		t.Fatalf("overlapping cells ran at %v, want two different paths", paths)
	}

	stable := 0

	for _, p := range paths {
		if strings.Contains(p, string(filepath.Separator)+"steps-work"+string(filepath.Separator)) {
			stable++
		}
	}

	if stable != 1 {
		t.Errorf("overlapping cells ran at %v, want exactly one at the stable path", paths)
	}
}
