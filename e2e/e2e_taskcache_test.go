package e2e

// caches: a directory a step keeps between runs, which only makes it faster and never changes what it is.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/testsshd"
)

// taskCachePipeline counts its runs in a cache dir; --force and volatile: because a skipped step never runs, so would never touch the cache.
func taskCachePipeline(t *testing.T, dir, root, report string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy
  root: %[1]s

jobs:
- name: build
  plan:
  - task: compile
    volatile: true
    caches:
    - path: .gocache
    outputs: [bin]
    run: |
      n=$(cat .gocache/runs 2>/dev/null || echo 0)
      n=$((n + 1))
      echo "$n" > .gocache/runs
      echo "$n" >> %[2]s
      echo built > bin/app
`, root, report))
}

// TestEndToEndTaskCacheSurvivesToTheNextRun: the cache survives, and a warm cache never moves the node hash.
func TestEndToEndTaskCacheSurvivesToTheNextRun(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")
	path := taskCachePipeline(t, dir, filepath.Join(dir, "ws"), report)

	mustRun(t, "run", path, "--force")
	mustRun(t, "run", path, "--force")

	got := readFileString(t, report)
	if got != "1\n2\n" {
		t.Errorf("runs reported %q, want the second run to see the first's cache (\"1\\n2\\n\")", got)
	}

	db := openStateDB(t, path)

	var hashes int

	err := db.QueryRowContext(t.Context(), `SELECT COUNT(DISTINCT hash) FROM nodes WHERE resource = 'compile'`).Scan(&hashes)
	if err != nil {
		t.Fatalf("query node hashes: %v", err)
	}

	if hashes != 1 {
		t.Errorf("compile recorded %d distinct node hashes, want 1 — the cache directory reached the step's identity", hashes)
	}
}

// TestEndToEndAgentCacheSurvivesToTheNextRun: an agent's run_shell is where self-build's compiles happen, so its caches: must persist the same way.
func TestEndToEndAgentCacheSurvivesToTheNextRun(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")
	count := `n=$(cat .cache/runs 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > .cache/runs; echo "$n" >> ` + report

	run := []turn{callsTool("run_shell", map[string]any{"command": count}), says("done")}
	fake := newFakeLLM(t, append(run, run...)...)

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  root: %[2]s

defaults:
  preflight:
    disabled: true

agents:
- name: builder
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]
  caches:
  - path: .cache

jobs:
- name: build
  plan:
  - agent: builder
    volatile: true
    messages:
      - Build it.
`, fake.URL, filepath.Join(dir, "ws")))

	mustRun(t, "run", path, "--force")
	mustRun(t, "run", path, "--force")

	if got := readFileString(t, report); got != "1\n2\n" {
		t.Errorf("runs reported %q, want the second run to see the first's cache (\"1\\n2\\n\")", got)
	}
}

// TestEndToEndPlacedTaskCacheStaysOnItsWorker: the local stand-in stays empty, so a warm second run can only have come from the worker's own volume.
func TestEndToEndPlacedTaskCacheStaysOnItsWorker(t *testing.T) {
	requireDockerE2E(t)
	t.Cleanup(func() { removeTaskCacheVolumes(t) })

	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")
	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  root: %[1]s

jobs:
- name: build
  plan:
  - task: compile
    tags: [box]
    image: `+dockerE2EImage+`
    volatile: true
    caches:
    - path: .gocache
    outputs: [out]
    run: |
      n=$(cat .gocache/runs 2>/dev/null || echo 0)
      n=$((n + 1))
      echo "$n" > .gocache/runs
      echo "$n" > out/runs
  - task: publish
    volatile: true
    inputs: [out]
    run: cat out/runs >> %[2]s
`, filepath.Join(dir, "ws"), report))

	mustRun(t, "run", path, "--force", "--worker", "box=local:")
	mustRun(t, "run", path, "--force", "--worker", "box=local:")

	if got := readFileString(t, report); got != "1\n2\n" {
		t.Errorf("runs reported %q, want the second run to see the worker's cache (\"1\\n2\\n\")", got)
	}
}

// removeTaskCacheVolumes drops the cache volumes this process made, which by design nothing else ever removes.
func removeTaskCacheVolumes(t *testing.T) {
	t.Helper()

	docker, err := dockerapi.New("")
	if err != nil {
		t.Logf("cleanup: %v", err)

		return
	}

	defer func() { _ = docker.Close() }()

	ctx := context.WithoutCancel(t.Context())

	volumes, err := docker.ListVolumes(ctx, map[string]string{"steps.pid": strconv.Itoa(os.Getpid())})
	if err != nil {
		t.Logf("cleanup: %v", err)

		return
	}

	for _, volume := range volumes {
		if strings.HasPrefix(volume.Name, "steps-tc-") {
			_ = docker.RemoveVolume(ctx, volume.Name)
		}
	}
}

// TestEndToEndSSHWorkerTaskCacheStaysOnItsWorker: a bare ssh:// worker keeps its caches under its root, beside the builds it removes.
func TestEndToEndSSHWorkerTaskCacheStaysOnItsWorker(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")
	worker := testsshd.New(t).URL
	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  root: %[1]s

jobs:
- name: build
  plan:
  - task: compile
    tags: [box]
    volatile: true
    caches:
    - path: .gocache
    outputs: [out]
    run: |
      n=$(cat .gocache/runs 2>/dev/null || echo 0)
      n=$((n + 1))
      echo "$n" > .gocache/runs
      echo "$n" > out/runs
  - task: publish
    volatile: true
    inputs: [out]
    run: cat out/runs >> %[2]s
`, filepath.Join(dir, "ws"), report))

	mustRun(t, "run", path, "--force", "--worker", "box="+worker)
	mustRun(t, "run", path, "--force", "--worker", "box="+worker)

	if got := readFileString(t, report); got != "1\n2\n" {
		t.Errorf("runs reported %q, want the second run to see the worker's cache (\"1\\n2\\n\")", got)
	}
}

// TestEndToEndPlacedAgentCacheStaysOnItsWorker crosses the seam the task test does not: the agent builds its own runner spec, which must carry the keys to the worker.
func TestEndToEndPlacedAgentCacheStaysOnItsWorker(t *testing.T) {
	requireDockerE2E(t)
	t.Cleanup(func() { removeTaskCacheVolumes(t) })

	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")
	count := `n=$(cat .cache/runs 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > .cache/runs; echo "$n" > out/runs`
	run := []turn{callsTool("run_shell", map[string]any{"command": count}), says("done")}
	fake := newFakeLLM(t, append(run, run...)...)

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  root: %[3]s

defaults:
  preflight:
    disabled: true

agents:
- name: builder
  image: %[2]s
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]
  caches:
  - path: .cache

jobs:
- name: build
  plan:
  - agent: builder
    tags: [box]
    volatile: true
    outputs: [out]
    messages:
      - Build it.
  - task: publish
    volatile: true
    inputs: [out]
    run: cat out/runs >> %[4]s
`, fake.URL, dockerE2EImage, filepath.Join(dir, "ws"), report))

	mustRun(t, "run", path, "--force", "--worker", "box=local:")
	mustRun(t, "run", path, "--force", "--worker", "box=local:")

	if got := readFileString(t, report); got != "1\n2\n" {
		t.Errorf("runs reported %q, want the second run to see the worker's cache (\"1\\n2\\n\")", got)
	}
}
