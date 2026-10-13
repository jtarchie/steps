package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A daemon polling many pipelines that watch the same thing ran the same check
// once per pipeline per interval: a hundred pipelines on one repository were a
// hundred identical `git ls-remote`s. A check that renders to the same command,
// in the same environment, is run once and its answer handed to each. Each
// pipeline still records its own history and triggers its own jobs.
func TestIdenticalChecksRunOncePerWindow(t *testing.T) {
	dir := t.TempDir()
	checks := filepath.Join(dir, "checks.log")
	ran := filepath.Join(dir, "ran.log")

	same := sharedCheckPipeline(t, dir, "same.yml", checks, ran, "shared")
	other := sharedCheckPipeline(t, dir, "other.yml", checks, ran, "different")

	served := startWebFor(t, same, "--interval", "1h")
	defer served.stop(t)

	served.set(t, "again", same)
	served.set(t, "other", other)

	// Every pipeline triggers its own build from the answer, shared or not.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && countLines(t, ran) < 3 {
		time.Sleep(20 * time.Millisecond)
	}

	if got := countLines(t, ran); got != 3 {
		t.Fatalf("%d of 3 pipelines built", got)
	}

	// A build checks for itself, and is never handed a shared answer: a
	// person who triggers right after a push wants what exists now. So each
	// pipeline's build adds one, and only the polls share: 2 builds + 1 poll.
	if got := linesEqual(t, checks, "shared"); got != 3 {
		t.Errorf("the shared check ran %d times for two pipelines, want 3 (one poll, two builds)", got)
	}

	if got := linesEqual(t, checks, "different"); got != 2 {
		t.Errorf("the distinct check ran %d times, want 2 (its poll and its build)", got)
	}
}

func sharedCheckPipeline(t *testing.T, dir, name, checks, ran, id string) string {
	t.Helper()

	path := filepath.Join(dir, name)

	body := fmt.Sprintf(`resource_types:
- name: counter
  config:
    check: "echo {{ .source.id }} >> %s; echo '[{\"n\": \"1\"}]'"
    in: "true"
resources:
- name: thing
  type: counter
  source: {id: %s}
jobs:
- name: build
  plan:
  - get: thing
    trigger: true
  - task: note
    run: echo built >> %s
`, checks, id, ran)

	err := os.WriteFile(path, []byte(body), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}
