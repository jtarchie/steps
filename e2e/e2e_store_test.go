package e2e

// The host's content-addressed store (#259 phase 2): every tree a cache keeps on this machine is filed once, by digest, under <root>/store, and the caches hold pointers at it.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// storePipeline is two jobs whose different commands write the same bytes, each followed by a volatile reader that records what it was handed.
func storePipeline(t *testing.T, dir, ran, seen string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: a
  plan:
  - task: make
    outputs: [out]
    run: |
      echo a >> %[1]s
      printf 'same bytes\n' > out/file
  - task: read
    volatile: true
    inputs: [out]
    run: cat out/file >> %[2]s
- name: b
  plan:
  - task: make-too
    outputs: [out]
    run: |
      echo b >> %[1]s
      echo 'same bytes' > out/file
`, ran, seen))
}

// storeTrees lists the trees filed in root's store, skipping anything staged.
func storeTrees(t *testing.T, root string) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(root, "store"))
	if err != nil {
		t.Fatalf("reading the store: %v", err)
	}

	var trees []string

	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") && !strings.Contains(entry.Name(), "~") {
			trees = append(trees, entry.Name())
		}
	}

	return trees
}

// TestStoreKeepsOneTreeForIdenticalOutputs: two steps that produce the same bytes cost one tree on disk, and a hit on either restores it.
func TestStoreKeepsOneTreeForIdenticalOutputs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ran, seen := filepath.Join(dir, "ran.log"), filepath.Join(dir, "seen.log")
	path := storePipeline(t, dir, ran, seen)
	root := wsRoot(path)

	mustRun(t, "run", path, "--job", "a", "--workspace-root", root)
	mustRun(t, "run", path, "--job", "b", "--workspace-root", root)

	if got := storeTrees(t, root); len(got) != 1 {
		t.Fatalf("store holds %d trees %v after two steps wrote the same bytes, want 1", len(got), got)
	}

	var entries, digests int

	err := openStateDB(t, path).QueryRowContext(t.Context(),
		`SELECT COUNT(DISTINCT key), COUNT(DISTINCT digest) FROM cache_entry_trees WHERE kind = 'step' AND holder = ''`).Scan(&entries, &digests)
	if err != nil || entries != 2 || digests != 1 {
		t.Fatalf("the index holds %d entries naming %d trees (%v), want both steps naming one tree", entries, digests, err)
	}

	editReader(t, path, seen)
	mustRun(t, "run", path, "--job", "a", "--workspace-root", root)

	if got := readFileString(t, ran); got != "a\nb\n" {
		t.Errorf("steps that ran = %q, want make answered from the store on the rerun", got)
	}

	if got := readFileString(t, seen); got != "same bytes\nsame bytes\n" {
		t.Errorf("the reader saw %q, want the stored bytes both times", got)
	}
}

// TestStoreTamperedTreeIsAMiss: a stored tree whose bytes no longer match its digest is never handed to a step; the step runs, and the build is green.
func TestStoreTamperedTreeIsAMiss(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ran, seen := filepath.Join(dir, "ran.log"), filepath.Join(dir, "seen.log")
	path := storePipeline(t, dir, ran, seen)
	root := wsRoot(path)

	mustRun(t, "run", path, "--job", "a", "--workspace-root", root)

	trees := storeTrees(t, root)
	if len(trees) != 1 {
		t.Fatalf("store holds %v, want one tree", trees)
	}

	err := os.WriteFile(filepath.Join(root, "store", trees[0], "file"), []byte("tampered\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	editReader(t, path, seen)
	mustRun(t, "run", path, "--job", "a", "--workspace-root", root)

	if got := readFileString(t, ran); got != "a\na\n" {
		t.Errorf("steps that ran = %q, want make to run again over a tampered tree", got)
	}

	if got := readFileString(t, seen); got != "same bytes\nsame bytes\n" {
		t.Errorf("the reader saw %q, want the step's own bytes, never the tampered ones", got)
	}

	data, err := os.ReadFile(filepath.Join(root, "store", trees[0], "file")) //nolint:gosec // under the test's own temp dir
	if err != nil || string(data) != "same bytes\n" {
		t.Errorf("the store's tree after the rerun = %q (%v), want the tampered copy replaced", data, err)
	}
}

// TestStoreIsSharedAcrossPipelinesButStepEntriesAreNot: the index lives in the state database, but a step's entry is filed under its pipeline, whose secrets and caches: its key does not hold, so a second pipeline's same step runs; the bytes it makes are the same tree in the store, kept once.
func TestStoreIsSharedAcrossPipelinesButStepEntriesAreNot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ran, seen := filepath.Join(dir, "ran.log"), filepath.Join(dir, "seen.log")
	state := filepath.Join(dir, "shared.db")
	root := filepath.Join(dir, "ws")

	first := storePipeline(t, t.TempDir(), ran, seen)
	second := storePipeline(t, t.TempDir(), ran, seen)

	mustRun(t, "run", first, "--job", "a", "--workspace-root", root, "--db", state)
	mustRun(t, "run", second, "--job", "a", "--workspace-root", root, "--db", state)

	if got := readFileString(t, ran); got != "a\na\n" {
		t.Errorf("steps that ran = %q, want the second pipeline's make run, not answered by the first's", got)
	}

	if trees := storeTrees(t, root); len(trees) != 1 {
		t.Errorf("store holds %v, want the one tree both pipelines made", trees)
	}

	if got := readFileString(t, seen); got != "same bytes\nsame bytes\n" {
		t.Errorf("the readers saw %q, want the stored bytes both times", got)
	}

	_, err := os.Stat(filepath.Join(root, "steps-cache"))
	if err == nil {
		t.Error("steps-cache/ exists: the index belongs in the state database")
	}
}

// TestStoreEvictsTheLeastRecentlyUsedTreePastItsByteBound: --store-max-bytes bounds the store as the worker cache is bounded, oldest use first; the step whose tree went runs again, and the build is green.
func TestStoreEvictsTheLeastRecentlyUsedTreePastItsByteBound(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ran, seen := filepath.Join(dir, "ran.log"), filepath.Join(dir, "seen.log")
	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: a
  plan:
  - task: make-a
    outputs: [out]
    run: |
      echo a >> %[1]s
      head -c 60 /dev/zero | tr '\0' a > out/file
  - task: read
    volatile: true
    inputs: [out]
    run: wc -c < out/file >> %[2]s
- name: b
  plan:
  - task: make-b
    outputs: [out]
    run: |
      echo b >> %[1]s
      head -c 60 /dev/zero | tr '\0' b > out/file
`, ran, seen))
	root := wsRoot(path)
	bound := []string{"--workspace-root", root, "--store-max-bytes", "100"}

	mustRun(t, append([]string{"run", path, "--job", "a"}, bound...)...)
	mustRun(t, append([]string{"run", path, "--job", "b"}, bound...)...)

	if got := storeTrees(t, root); len(got) != 1 {
		t.Fatalf("store holds %d trees %v of 60 bytes each under a 100 byte bound, want 1", len(got), got)
	}

	rewritePipeline(t, path, "run: wc -c < out/file >> "+seen+"\n", "run: wc -c < out/file >> "+seen+" && true\n")
	mustRun(t, append([]string{"run", path, "--job", "a"}, bound...)...)

	if got := readFileString(t, ran); got != "a\nb\na\n" {
		t.Errorf("steps that ran = %q, want make-a run again: its tree was evicted", got)
	}

	if got := strings.Fields(readFileString(t, seen)); len(got) != 2 || got[1] != "60" {
		t.Errorf("the reader saw %v, want the 60 bytes make-a wrote, both times", got)
	}
}

// TestStepsRunKeepsItsCachesWithoutARoot: steps run's default root is workspace/ beside its state database, as steps web's is, so the step cache works with no flag.
func TestStepsRunKeepsItsCachesWithoutARoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ran, seen := filepath.Join(dir, "ran.log"), filepath.Join(dir, "seen.log")
	path := storePipeline(t, dir, ran, seen)

	mustRun(t, "run", path, "--job", "a")

	// A changed reader keeps the chain skip from answering for the whole job, so make has to come from the step cache.
	rewritePipeline(t, path, "run: cat out/file >> "+seen, "run: cat out/file >> "+seen+" && true")
	mustRun(t, "run", path, "--job", "a")

	if got := readFileString(t, ran); got != "a\n" {
		t.Errorf("steps that ran = %q, want make answered by the step cache on the second run", got)
	}

	if got := readFileString(t, seen); got != "same bytes\nsame bytes\n" {
		t.Errorf("the reader saw %q, want the stored bytes both times", got)
	}

	if trees := storeTrees(t, filepath.Join(dir, ".steps", "workspace")); len(trees) != 1 {
		t.Errorf("the default root's store holds %v, want make's one tree", trees)
	}
}

// TestForceRunsACachedStepAgainAndRefilesIt: --force asks the step cache nothing, so every step runs, but what it makes is still filed, so the next unforced run is a hit again.
func TestForceRunsACachedStepAgainAndRefilesIt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ran, seen := filepath.Join(dir, "ran.log"), filepath.Join(dir, "seen.log")
	path := storePipeline(t, dir, ran, seen)

	mustRun(t, "run", path, "--job", "a")

	// Only the forced run's filing can answer the last run.
	err := evictLocalCache(filepath.Join(dir, ".steps", "workspace"))
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, "run", path, "--job", "a", "--force")

	if got := readFileString(t, ran); got != "a\na\n" {
		t.Errorf("steps that ran = %q, want make run again under --force", got)
	}

	editReader(t, path, seen)
	mustRun(t, "run", path, "--job", "a")

	if got := readFileString(t, ran); got != "a\na\n" {
		t.Errorf("steps that ran = %q, want the forced run's outputs answering the next run", got)
	}
}

// TestAResumeUnderAnExplicitRootFindsItsTree: a failed run keeps its build tree for --resume, and the next steps run under the same --workspace-root must not sweep it away as a crashed process's leftovers.
func TestAResumeUnderAnExplicitRootFindsItsTree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	flag, made := filepath.Join(dir, "fail"), filepath.Join(dir, "made.log")
	root := filepath.Join(dir, "ws")
	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: make
    outputs: [out]
    run: |
      echo made >> %[2]s
      echo made > out/file
  - task: check
    inputs: [out]
    run: test ! -f %[1]s && cat out/file
`, flag, made))

	err := os.WriteFile(flag, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_ = captureStdout(t, func() {
		if stepsRun(t, []string{"run", path, "--workspace-root", root}) == nil {
			t.Fatal("expected the check to fail")
		}
	})

	runID := resumeIDFrom(t, path)

	err = os.Remove(flag)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, "run", path, "--workspace-root", root, "--resume", runID)

	if got := readFileString(t, made); got != "made\n" {
		t.Errorf("make ran %q, want once: the resume continues the failed run in its kept tree", got)
	}
}

// editReader changes storePipeline's reader so the chain skip cannot answer for the job, and make has to come from the step cache.
func editReader(t *testing.T, path, seen string) {
	t.Helper()

	rewritePipeline(t, path, "run: cat out/file >> "+seen+"\n", "run: cat out/file >> "+seen+" && true\n")
}

// evictLocalCache drops this machine's store, the index in the state database surviving: the shape of a second machine handed that database.
func evictLocalCache(root string) error {
	return os.RemoveAll(filepath.Join(root, "store")) //nolint:wrapcheck // a test helper
}
