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

	assertNoCachedBytes(t, filepath.Join(root, "steps-cache"))

	// --force bypasses only the chain skip, so make is answered by the step cache, from the store.
	mustRun(t, "run", path, "--job", "a", "--workspace-root", root, "--force")

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

	mustRun(t, "run", path, "--job", "a", "--workspace-root", root, "--force")

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

// evictLocalCache drops this machine's cached step outputs, pointers and trees both: the shape of a second machine handed the state database.
func evictLocalCache(root string) error {
	for _, dir := range []string{"steps-cache", "store"} {
		err := os.RemoveAll(filepath.Join(root, dir))
		if err != nil {
			return err //nolint:wrapcheck // a test helper
		}
	}

	return nil
}

// assertNoCachedBytes fails when a cache directory holds anything but pointers: the bytes belong in the store, once.
func assertNoCachedBytes(t *testing.T, dir string) {
	t.Helper()

	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !entry.IsDir() && entry.Name() != ".held.json" {
			t.Errorf("%s holds bytes (%s), want only pointers into the store", dir, path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
}
