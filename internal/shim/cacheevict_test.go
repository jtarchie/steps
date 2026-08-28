package shim

// What an eviction is allowed to leave behind.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCacheTree lays out one cached artifact from a path -> contents map and
// answers its path. Its sibling in artifactcache_test.go makes an entry of a
// given SIZE; what matters here is which files it holds.
func writeCacheTree(t *testing.T, cache, digest string, files map[string]string) string {
	t.Helper()

	entry := filepath.Join(cache, digest)

	for name, content := range files {
		path := filepath.Join(entry, name)

		err := os.MkdirAll(filepath.Dir(path), 0o700)
		if err != nil {
			t.Fatalf("making %s: %v", filepath.Dir(path), err)
		}

		err = os.WriteFile(path, []byte(content), 0o600)
		if err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}

	return entry
}

// TestEvictionNeverLeavesAPartialEntryUnderItsDigest is the poisoning this
// closes, and it is worth being precise about why it poisons rather than
// merely losing data.
//
// A cache entry is named by the digest of its content, and both readers ask
// only whether that name EXISTS: placeIfHeld stats it and copies, and the
// offer it answers tells the orchestrator to send nothing. So an entry that
// exists but is incomplete is served as whole, to every later step that asks
// for that digest, and the re-fetch that would repair it is exactly what the
// false hit prevents. Nothing re-reads a content-addressed cache, so it never
// heals.
//
// RemoveAll is not atomic. Interrupted — a shim SIGKILLed by an OOM or a spot
// reclamation, or simply a child it cannot unlink — it leaves some of the
// tree behind under the real name. Renaming the entry out of the way FIRST
// makes that impossible: a rename either happened or it did not, so a name a
// reader can see is always a complete entry.
func TestEvictionNeverLeavesAPartialEntryUnderItsDigest(t *testing.T) {
	t.Parallel()

	cache := t.TempDir()

	// An entry RemoveAll cannot finish: the codec restores recorded modes, so
	// a tree carrying a read-only directory is ordinary rather than contrived
	// — and it is the case the sweep's own comment already names.
	digest := strings.Repeat("a", 64)
	entry := writeCacheTree(t, cache, digest, map[string]string{
		"keep/one.txt":   "first",
		"keep/two.txt":   "second",
		"locked/gem.txt": "the file RemoveAll cannot unlink",
	})

	locked := filepath.Join(entry, "locked")

	err := os.Chmod(locked, 0o500) //nolint:gosec // an undeletable entry is the condition under test
	if err != nil {
		t.Fatalf("locking %s: %v", locked, err)
	}

	// Unlocked by walking, not by path: eviction RENAMES, so the directory
	// this test locked is somewhere else by the time cleanup runs — which is
	// the property being tested.
	t.Cleanup(func() { unlockTree(cache) })

	err = evictArtifact(entry)

	// Whether the eviction could finish is not the point — a disk that
	// refuses a delete is a disk problem. What must hold either way is that
	// no reader can see a half-entry.
	_, statErr := os.Stat(filepath.Join(cache, digest))
	if statErr == nil {
		assertEntryIsWhole(t, filepath.Join(cache, digest))
	}

	// And the bytes it could not free are still ACCOUNTED for: left under a
	// name the sweep recognizes, not lost to it. An orphan the cap cannot see
	// is how a bounded cache stops being bounded.
	if err != nil && !cacheHoldsTombstone(t, cache) {
		t.Error("an eviction that could not finish left nothing the sweep can find or count")
	}
}

// assertEntryIsWhole fails unless every file the entry was created with is
// still there.
func assertEntryIsWhole(t *testing.T, entry string) {
	t.Helper()

	for _, name := range []string{"keep/one.txt", "keep/two.txt", "locked/gem.txt"} {
		_, err := os.Stat(filepath.Join(entry, name))
		if err != nil {
			t.Errorf("the entry is still visible under its digest but %s is gone — a later step would take this as the whole artifact and send nothing for it", name)
		}
	}
}

// cacheHoldsTombstone reports whether anything in cache is named as an
// eviction in progress.
func cacheHoldsTombstone(t *testing.T, cache string) bool {
	t.Helper()

	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatalf("reading the cache: %v", err)
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), tombstonePrefix) {
			return true
		}
	}

	return false
}

// TestSweepCountsAndClearsAbandonedTombstones: a tombstone is garbage a
// killed sweep left, so the next one must both SEE its bytes — a cap that
// cannot see them stops bounding the disk, which is the same accounting bug
// staging directories already had — and take another run at removing it.
func TestSweepCountsAndClearsAbandonedTombstones(t *testing.T) {
	t.Parallel()

	cache := t.TempDir()

	abandoned := writeCacheTree(t, cache, tombstonePrefix+"abandoned", map[string]string{
		"big.bin": strings.Repeat("x", 4096),
	})

	live := writeCacheTree(t, cache, strings.Repeat("b", 64), map[string]string{
		"small.txt": "kept",
	})

	err := sweepArtifactCache(cache)
	if err != nil {
		t.Fatalf("sweepArtifactCache: %v", err)
	}

	_, statErr := os.Stat(abandoned)
	if statErr == nil {
		t.Error("an abandoned tombstone survived a sweep — nothing else will ever remove it")
	}

	// Under the cap, so the live entry is untouched: a sweep that reclaims
	// garbage must not also start evicting.
	_, statErr = os.Stat(live)
	if statErr != nil {
		t.Error("the sweep evicted a live entry while the cache was under its cap")
	}
}

// unlockTree makes every directory under root removable again, so the test's
// own TempDir cleanup can finish.
func unlockTree(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			_ = os.Chmod(path, 0o700) //nolint:gosec // restoring what the test narrowed
		}

		return nil
	})
}
