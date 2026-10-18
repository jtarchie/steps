package e2e

// caches: a directory a step keeps between runs, which only makes it faster and never changes what it is.

import (
	"fmt"
	"path/filepath"
	"testing"
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
