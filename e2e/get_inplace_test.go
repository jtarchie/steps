package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestASecondEveryGetWithNothingNewIsIdleNotBroken: when the get fanning out is not the build's first, a rerun with every version taken is still "nothing new" — not a warning that the first get's check came back empty, which it did not.
func TestASecondEveryGetWithNothingNewIsIdleNotBroken(t *testing.T) {
	dir := t.TempDir()
	feedA := filepath.Join(dir, "a.json")
	feedB := filepath.Join(dir, "b.json")

	writePipelineFile(t, feedA, `[{"n":"1"}]`)
	writePipelineFile(t, feedB, `[{"n":"1"}]`)

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: feed
  config:
    check: cat {{ .source.file }}
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: a
  type: feed
  source: {file: %s}
- name: b
  type: feed
  source: {file: %s}
jobs:
- name: build
  plan:
  - get: a
  - get: b
    version: every
  - task: use
    inputs: [b]
    run: cat b/n.txt
`, feedA, feedB))

	mustRun(t, "run", path, "--job", "build")

	out := captureStdout(t, func() { mustRun(t, "run", path, "--job", "build") })

	if strings.Contains(out, "warning:") || !strings.Contains(out, "all 1 already taken") {
		t.Errorf("a rerun with nothing new said:\n%s\nwant it reported as idle, every version already taken", out)
	}
}

// TestANewVersionOfASecondGetRerunsWhatFollowsIt: the steps after a build's second get hang off that get's node, so a new version of it — the first get unchanged — is new work, not a cache hit on the last build's.
func TestANewVersionOfASecondGetRerunsWhatFollowsIt(t *testing.T) {
	dir := t.TempDir()
	feedA := filepath.Join(dir, "a.json")
	feedB := filepath.Join(dir, "b.json")
	seen := filepath.Join(dir, "seen.log")

	writePipelineFile(t, feedA, `[{"n":"1"}]`)
	writePipelineFile(t, feedB, `[{"n":"1"}]`)

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: feed
  config:
    check: cat {{ .source.file }}
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: a
  type: feed
  source: {file: %s}
- name: b
  type: feed
  source: {file: %s}
jobs:
- name: build
  plan:
  - get: a
  - get: b
  - task: use
    inputs: [b]
    run: cat b/n.txt >> %s
`, feedA, feedB, seen))

	mustRun(t, "run", path, "--job", "build")
	mustRun(t, "run", path, "--job", "build")
	assertLineCount(t, seen, 1)

	writePipelineFile(t, feedB, `[{"n":"1"},{"n":"2"}]`)
	mustRun(t, "run", path, "--job", "build")

	if got := readFileString(t, seen); got != "1\n2\n" {
		t.Errorf("seen.log = %q, want the task to have run again for b's new version", got)
	}
}

// TestResumeRefusesWhenAnInPlaceGetsKeptArtifactIsGone: the second get of a build keeps its artifact on resume by the same rule as the first, so it refuses by the same rule too, rather than carrying on without what the skipped steps changed.
func TestResumeRefusesWhenAnInPlaceGetsKeptArtifactIsGone(t *testing.T) {
	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	writePipelineFile(t, versions, `[{"n":"two"}]`)

	path := writePipeline(t, dir, artifactResumePipeline(dir, versions, "  - get: a\n    resource: ticks\n  - get: repo\n    resource: ticks", ""))

	runID, root := failThenCapture(t, path)

	err := os.RemoveAll(filepath.Join(root, "artifacts", "repo"))
	if err != nil {
		t.Fatal(err)
	}

	writePipelineFile(t, filepath.Join(dir, "fixed"), "")

	out := captureStdout(t, func() {
		err = cli.Run([]string{"run", path, "--resume", runID})
	})

	if err == nil {
		t.Fatalf("the resume continued without the artifact the skipped steps changed:\n%s", out)
	}

	if !strings.Contains(err.Error(), `get "repo" was already fetched`) {
		t.Errorf("the refusal does not name the in-place get: %v", err)
	}
}
