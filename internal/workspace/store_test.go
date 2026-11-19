package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// storeTreesIn lists the digests filed in a provider's store.
func storeTreesIn(t *testing.T, store *treeStore) []string {
	t.Helper()

	entries, err := os.ReadDir(store.dir)
	if err != nil {
		t.Fatal(err)
	}

	var trees []string

	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			trees = append(trees, entry.Name())
		}
	}

	return trees
}

// fileStep runs one step whose output holds content and files it, from a fresh build of provider.
func fileStep(t *testing.T, provider Provider, contentHash, content string) {
	t.Helper()

	req := stepRequest()
	req.ContentHash = contentHash
	bw := seededBuild(t, provider, "notes")
	res := mustRestore(t, bw, req)
	produceOutput(t, bw, req, content)

	err := bw.StoreStep(context.Background(), res.Key, req)
	if err != nil {
		t.Fatal(err)
	}
}

func TestStoreSweepsATreeNoEntryNamesAnyMore(t *testing.T) {
	t.Parallel()

	provider := stepCacheProvider(t, t.TempDir())
	p := provider.(*isolatingProvider)
	p.stepCache.maxEntries = 1
	p.store.grace = 0

	fileStep(t, provider, "first", "first bytes")

	before := storeTreesIn(t, p.store)
	if len(before) != 1 {
		t.Fatalf("store after one step = %v, want one tree", before)
	}

	fileStep(t, provider, "second", "second bytes")

	after := storeTreesIn(t, p.store)
	if len(after) != 1 || after[0] == before[0] {
		t.Fatalf("store after the first entry was evicted = %v, want only the second step's tree (the first was %s)", after, before[0])
	}
}

func TestStoreSweepSparesATreeYoungerThanItsGrace(t *testing.T) {
	t.Parallel()

	provider := stepCacheProvider(t, t.TempDir())
	p := provider.(*isolatingProvider)
	src := t.TempDir()

	err := os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	digest, err := p.store.put(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}

	// Released by this index while another process sharing the root has just filed the same bytes, which this one cannot see.
	p.store.release([]string{digest})

	if !p.store.has(digest) {
		t.Fatal("a tree filed a moment ago was removed before anything could index it")
	}
}

// TestADurableRootDropsTheOldWholeTreeCaches: nothing reads cache/ and steps-cache/ since the store and the index replaced them, and they can hold gigabytes.
func TestADurableRootDropsTheOldWholeTreeCaches(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	for _, dir := range []string{"cache", "steps-cache"} {
		err := os.MkdirAll(filepath.Join(root, dir, "entry"), 0o750)
		if err != nil {
			t.Fatal(err)
		}
	}

	stepCacheProvider(t, root)

	for _, dir := range []string{"cache", "steps-cache"} {
		_, err := os.Stat(filepath.Join(root, dir))
		if err == nil {
			t.Errorf("%s/ survived; nothing reads it any more", dir)
		}
	}
}

func TestStoreRefusesADigestThatIsAPath(t *testing.T) {
	t.Parallel()

	store, err := newTreeStore(copyBackend{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, digest := range []string{"", "..", "../escape", "ab/cd", "ABCD"} {
		_, pathErr := store.path(digest)
		if pathErr == nil {
			t.Errorf("path(%q) was accepted, want it refused", digest)
		}
	}
}
