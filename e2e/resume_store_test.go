package e2e

// Resume from the store (#259 phase 4): a failed run files what its build held into the store and keeps only the record of it, and a resume clones its inputs back from those digests.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAFailedRunsArtifactsWaitInTheStoreForItsResume is #190's shape: step 2 changes the get's artifact, step 3 fails. The failed run's tree keeps no copy of the artifact, only the record of its digest; the resume skips steps 1-2 and the retried step reads step 2's change, cloned straight from the store.
func TestAFailedRunsArtifactsWaitInTheStoreForItsResume(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	writePipelineFile(t, versions, `[{"n":"two"}]`)

	path := writePipeline(t, dir, artifactResumePipeline(dir, versions, "  - get: repo\n    resource: ticks", ""))

	runID, root := failThenCapture(t, path)

	artifacts, err := os.ReadDir(filepath.Join(root, "artifacts"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}

	if len(artifacts) != 0 {
		t.Errorf("the failed run's tree still holds %d artifacts, want them reclaimed into the store", len(artifacts))
	}

	held := readFileString(t, filepath.Join(root, "held.json"))
	if !strings.Contains(held, `"repo"`) {
		t.Errorf("held.json = %s, want the record of repo's digest", held)
	}

	writePipelineFile(t, filepath.Join(dir, "fixed"), "")

	err = stepsRun(t, []string{"run", "--workspace-root", t.TempDir(), path, "--resume", runID})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	assertLineCount(t, filepath.Join(dir, "fetches.log"), 1)
	assertLineCount(t, filepath.Join(dir, "change.log"), 1)

	if got := readFileString(t, filepath.Join(dir, "seen.log")); strings.Count(got, "changed two") != 2 {
		t.Errorf("the retried step did not see the change the skipped step made: seen.log = %q", got)
	}

	_, err = os.Stat(filepath.Join(root, "artifacts", "repo"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("artifacts/repo came back into the tree (%v), want the retried step's input cloned from the store", err)
	}
}

// TestAResumeThatFailsAgainFilesItsTreeAgain: a resume's job build and triggered build continue in one tree, and when the resume fails too, what the second attempt made must be filed beside what the first one filed, for the third to read both.
func TestAResumeThatFailsAgainFilesItsTreeAgain(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	writePipelineFile(t, versions, `[{"n":"two"}]`)

	tail := fmt.Sprintf(`  - task: more
    volatile: true
    outputs: [more]
    run: echo more > more/file
  - task: last
    inputs: [repo, more]
    run: test -f %[1]s && cat repo/changed.txt more/file >> %[2]s
`, filepath.Join(dir, "fixed-last"), filepath.Join(dir, "last.log"))

	path := writePipeline(t, dir, artifactResumePipeline(dir, versions, "  - get: repo\n    resource: ticks", tail))

	runID, _ := failThenCapture(t, path)

	writePipelineFile(t, filepath.Join(dir, "fixed"), "")

	err := stepsRun(t, []string{"run", path, "--resume", runID})
	if err == nil {
		t.Fatal("the first resume passed, want it to fail at last")
	}

	writePipelineFile(t, filepath.Join(dir, "fixed-last"), "")

	err = stepsRun(t, []string{"run", path, "--resume", runID})
	if err != nil {
		t.Fatalf("the second resume failed: %v", err)
	}

	if got := readFileString(t, filepath.Join(dir, "last.log")); got != "changed two\nmore\n" {
		t.Errorf("last read %q, want the first attempt's change and the second attempt's output", got)
	}

	assertLineCount(t, filepath.Join(dir, "fetches.log"), 1)
	assertLineCount(t, filepath.Join(dir, "change.log"), 1)
}

// TestAResumedImageStepIsSentWhatTheStoreHolds: a container step's input that the store holds has no worker to be offered from, so it is cloned here and sent like any input this machine has.
func TestAResumedImageStepIsSentWhatTheStoreHolds(t *testing.T) {
	t.Parallel()
	dockerSlot(t)

	requireDockerE2E(t)

	dir := t.TempDir()
	fixed := filepath.Join(dir, "fixed")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: make
    outputs: [src]
    run: |
      echo made > src/file
      test -f %[1]s || echo first >> %[2]s
  - task: gate
    run: test -f %[1]s
  - task: read
    image: %[3]s
    inputs: [src]
    outputs: [out]
    run: cp src/file out/file
  - task: publish
    inputs: [out]
    run: cp out/file %[4]s
`, fixed, filepath.Join(dir, "made.log"), dockerE2EImage, filepath.Join(dir, "published")))

	runID, _ := failThenCapture(t, path)

	writePipelineFile(t, fixed, "")

	err := stepsRun(t, []string{"run", path, "--resume", runID})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	if got := readFileString(t, filepath.Join(dir, "published")); got != "made\n" {
		t.Errorf("the image step was handed %q, want the bytes the skipped step filed", got)
	}

	assertLineCount(t, filepath.Join(dir, "made.log"), 1)
}

// TestKeepWorkspaceKeepsAFailedRunsArtifactsInItsTree: --keep-workspace is the request to look at the files, so a failed run under it files nothing away.
func TestKeepWorkspaceKeepsAFailedRunsArtifactsInItsTree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	writePipelineFile(t, versions, `[{"n":"two"}]`)

	path := writePipeline(t, dir, artifactResumePipeline(dir, versions, "  - get: repo\n    resource: ticks", ""))

	out := captureStdout(t, func() {
		err := stepsRun(t, []string{"run", path, "--job", "build", "--keep-workspace"})
		if err == nil {
			t.Fatal("expected the fragile step to fail")
		}
	})

	var root string

	err := openStateDB(t, path).QueryRowContext(t.Context(), `SELECT workspace FROM runs WHERE id = ?`, resumeID(t, out)).Scan(&root)
	if err != nil {
		t.Fatal(err)
	}

	if got := readFileString(t, filepath.Join(root, "artifacts", "repo", "changed.txt")); got != "changed two\n" {
		t.Errorf("artifacts/repo/changed.txt = %q, want the failed run's files kept in place", got)
	}
}

// reclaimStore removes every tree in the store a failed run's tree sits beside, as the store's byte bound would once the run's pins expire.
func reclaimStore(t *testing.T, root string) {
	t.Helper()

	err := os.RemoveAll(filepath.Join(filepath.Dir(root), "store"))
	if err != nil {
		t.Fatal(err)
	}
}
