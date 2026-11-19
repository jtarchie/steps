package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestStoreBoundSparesTheTreeJustFiled: a tree bigger than the whole bound is still kept until the next one is filed, or the entry about to name it would be a miss before anything could hit it.
func TestStoreBoundSparesTheTreeJustFiled(t *testing.T) {
	t.Parallel()

	store, err := newTreeStore(copyBackend{}, t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}

	digests := make([]string, 0, 2)

	for _, content := range []string{"sixty bytes, well past a ten byte bound, first", "sixty bytes, well past a ten byte bound, second"} {
		src := t.TempDir()

		err = os.WriteFile(filepath.Join(src, "f"), []byte(content), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		digest, putErr := store.put(context.Background(), src)
		if putErr != nil {
			t.Fatal(putErr)
		}

		digests = append(digests, digest)
	}

	if store.has(digests[0]) || !store.has(digests[1]) {
		t.Errorf("first kept = %v, second kept = %v; want only the tree just filed", store.has(digests[0]), store.has(digests[1]))
	}
}

// TestStoreBoundEvictsTheLeastRecentlyUsed: a hit refreshes a tree, so the one evicted is the one used longest ago, not the one filed first.
func TestStoreBoundEvictsTheLeastRecentlyUsed(t *testing.T) {
	t.Parallel()

	store, err := newTreeStore(copyBackend{}, t.TempDir(), 25)
	if err != nil {
		t.Fatal(err)
	}

	put := func(content string) string {
		t.Helper()

		src := t.TempDir()

		writeErr := os.WriteFile(filepath.Join(src, "f"), []byte(content), 0o600)
		if writeErr != nil {
			t.Fatal(writeErr)
		}

		digest, putErr := store.put(context.Background(), src)
		if putErr != nil {
			t.Fatal(putErr)
		}

		return digest
	}

	first := put("ten bytes1")
	second := put("ten bytes2")

	// first filed an hour ago, second a minute after it; then first is used, and the hit is all that keeps it.
	filed := time.Now().Add(-time.Hour)
	for i, digest := range []string{first, second} {
		at := filed.Add(time.Duration(i) * time.Minute)

		chErr := os.Chtimes(filepath.Join(store.dir, digest), at, at)
		if chErr != nil {
			t.Fatal(chErr)
		}
	}

	err = store.materialize(context.Background(), first, filepath.Join(t.TempDir(), "hit"))
	if err != nil {
		t.Fatal(err)
	}

	put("ten bytes3")

	if !store.has(first) || store.has(second) {
		t.Errorf("used first kept = %v, unused second kept = %v; want the least recently used evicted", store.has(first), store.has(second))
	}
}

// TestStoreMeasuresATreeWhoseSizeWasNotRecorded: a size record lost (or a tree from before them) is measured, not counted as nothing, or the bound would never see it.
func TestStoreMeasuresATreeWhoseSizeWasNotRecorded(t *testing.T) {
	t.Parallel()

	store, err := newTreeStore(copyBackend{}, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()

	err = os.WriteFile(filepath.Join(src, "f"), []byte("12345"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	digest, err := store.put(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Remove(filepath.Join(store.dir, sizesDirName, digest))
	if err != nil {
		t.Fatal(err)
	}

	if got := store.size(digest); got != 5 {
		t.Errorf("size = %d, want the 5 bytes the tree holds", got)
	}
}

func TestStoreRefusesADigestThatIsAPath(t *testing.T) {
	t.Parallel()

	store, err := newTreeStore(copyBackend{}, t.TempDir(), 0)
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
