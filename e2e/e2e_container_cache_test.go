package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// A block whose every branch is cacheable is cacheable. Every container used
// to make its whole job re-run on every build, the steps before it included,
// so a job with one in_parallel: never cached at all.
func TestContainersOfCacheableStepsCache(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran.log")

	pipeline := writeContainerPipeline(t, dir, log, `
  - task: first
    run: echo first >> `+log+`
  - in_parallel:
    - task: lint
      run: echo lint >> `+log+`
    - do:
      - task: test
        run: echo test >> `+log+`
  - race:
      steps:
      - task: fast
        run: echo fast >> `+log+`
      - task: slow
        run: sleep 5
  - task: last
    run: echo last >> `+log)

	for range 2 {
		err := cli.Run([]string{"run", pipeline, "--progress", "plain"})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	for _, step := range []string{"first", "lint", "test", "fast", "last"} {
		if got := linesEqual(t, log, step); got != 1 {
			t.Errorf("%s ran %d times across two runs, want 1 (the second is a cache hit)", step, got)
		}
	}
}

// A plan that opens with a block has no task in front of it to notice the
// chain already succeeded: the block itself asks the skip index.
func TestAPlanOpeningWithABlockCaches(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran.log")

	pipeline := writeContainerPipeline(t, dir, log, `
  - in_parallel:
    - task: lint
      run: echo lint >> `+log+`
    - task: vet
      run: echo vet >> `+log)

	for range 2 {
		err := cli.Run([]string{"run", pipeline, "--progress", "plain"})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	for _, step := range []string{"lint", "vet"} {
		if got := linesEqual(t, log, step); got != 1 {
			t.Errorf("%s ran %d times across two runs, want 1 (the second is a cache hit)", step, got)
		}
	}
}

// A block holding anything uncacheable still re-runs its chain: a put in a
// branch is a side effect the planner must not stand in for.
func TestAContainerWithAPutBranchStillReruns(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran.log")

	pipeline := writeContainerPipeline(t, dir, log, `
  - task: first
    run: echo first >> `+log+`
  - in_parallel:
    - task: lint
      run: echo lint >> `+log+`
    - put: sink`)

	for range 2 {
		err := cli.Run([]string{"run", pipeline, "--progress", "plain"})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	for _, step := range []string{"first", "lint"} {
		if got := linesEqual(t, log, step); got != 2 {
			t.Errorf("%s ran %d times across two runs, want 2 (a put branch makes the chain uncacheable)", step, got)
		}
	}
}

func writeContainerPipeline(t *testing.T, dir, log, plan string) string {
	t.Helper()

	path := filepath.Join(dir, "pipeline.yml")

	body := fmt.Sprintf(`resource_types:
- name: sink
  config:
    out: "echo out >> %s; echo '{\"v\": \"1\"}'"
resources:
- name: sink
  type: sink
  source: {}
jobs:
- name: build
  plan:%s
`, log, plan)

	err := os.WriteFile(path, []byte(body), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

func linesEqual(t *testing.T, path, line string) int {
	t.Helper()

	count := 0

	for got := range strings.SplitSeq(readFileString(t, path), "\n") {
		if got == line {
			count++
		}
	}

	return count
}
