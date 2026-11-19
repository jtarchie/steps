package storetest

// The cache index: which tree each cache entry names, and who holds it.

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestCacheEntryRoundTrip: an entry's trees by name, replaced wholesale on re-record so an output the step no longer declares does not survive as a stale row.
func (s suite) TestCacheEntryRoundTrip(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := ctxFor(t)

	got, err := st.CacheEntry(ctx, store.CacheStep, "unknown")
	if err != nil || len(got) != 0 {
		t.Fatalf("CacheEntry of an unknown key = %v, %v; want empty, nil", got, err)
	}

	first := map[string]store.CachedTree{"out": {Digest: "d1"}, "logs": {Name: "logs", Digest: "d2", Holder: "local:"}}

	_, err = st.RecordCacheEntry(ctx, store.CacheStep, "key-1", first, 10)
	if err != nil {
		t.Fatalf("RecordCacheEntry: %v", err)
	}

	assertCacheEntry(t, st, store.CacheStep, "key-1", first)

	second := map[string]store.CachedTree{"out": {Digest: "d3"}}

	_, err = st.RecordCacheEntry(ctx, store.CacheStep, "key-1", second, 10)
	if err != nil {
		t.Fatalf("RecordCacheEntry again: %v", err)
	}

	assertCacheEntry(t, st, store.CacheStep, "key-1", second)
	assertCacheEntry(t, st, store.CacheResource, "key-1", map[string]store.CachedTree{})
}

// TestCacheEntriesAreSharedByEveryPipeline: the index is machine-wide, as Concourse's resource caches are; the key already hashes everything that decides the bytes.
func (s suite) TestCacheEntriesAreSharedByEveryPipeline(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	first, second := s.open(t, "app"), s.open(t, "infra")
	trees := map[string]store.CachedTree{"out": {Digest: "app-digest"}}

	_, err := first.RecordCacheEntry(ctx, store.CacheStep, "same-key", trees, 10)
	if err != nil {
		t.Fatalf("RecordCacheEntry: %v", err)
	}

	assertCacheEntry(t, second, store.CacheStep, "same-key", trees)
}

// TestCacheEntriesEvictTheLeastRecentlyUsed: count, never age, and a touched entry outlives a newer untouched one; the evicted entry's tree is released, so the store may remove it.
func (s suite) TestCacheEntriesEvictTheLeastRecentlyUsed(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := ctxFor(t)

	for i := range 3 {
		_, err := st.RecordCacheEntry(ctx, store.CacheResource, fmt.Sprintf("key-%d", i), map[string]store.CachedTree{"tree": {Digest: fmt.Sprintf("d%d", i)}}, 3)
		if err != nil {
			t.Fatalf("RecordCacheEntry %d: %v", i, err)
		}
	}

	err := st.TouchCacheEntry(ctx, store.CacheResource, "key-0")
	if err != nil {
		t.Fatalf("TouchCacheEntry: %v", err)
	}

	released, err := st.RecordCacheEntry(ctx, store.CacheResource, "key-3", map[string]store.CachedTree{"tree": {Digest: "d3"}}, 3)
	if err != nil {
		t.Fatalf("RecordCacheEntry: %v", err)
	}

	assertCacheEntry(t, st, store.CacheResource, "key-1", map[string]store.CachedTree{})
	assertCacheEntry(t, st, store.CacheResource, "key-0", map[string]store.CachedTree{"tree": {Digest: "d0"}})

	if !slices.Equal(released, []string{"d1"}) {
		t.Errorf("released = %v, want the evicted entry's tree", released)
	}
}

// TestOnlyATreeNoEntryNamesIsReleased: a tree another entry still names, and a tree docker holds, are never the store's to remove.
func (s suite) TestOnlyATreeNoEntryNamesIsReleased(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := ctxFor(t)

	for _, key := range []string{"a", "b"} {
		_, err := st.RecordCacheEntry(ctx, store.CacheStep, key, map[string]store.CachedTree{"out": {Digest: "shared"}, "held": {Digest: "h-" + key, Holder: "ssh://box"}}, 10)
		if err != nil {
			t.Fatalf("RecordCacheEntry: %v", err)
		}
	}

	released, err := st.RecordCacheEntry(ctx, store.CacheStep, "a", map[string]store.CachedTree{"out": {Digest: "new"}}, 10)
	if err != nil || len(released) != 0 {
		t.Errorf("replacing a's trees released %v (%v), want nothing: b still names shared, and h-a was never in the store", released, err)
	}

	released, err = st.RecordCacheEntry(ctx, store.CacheStep, "b", map[string]store.CachedTree{"out": {Digest: "new"}}, 10)
	if err != nil || !slices.Equal(released, []string{"shared"}) {
		t.Errorf("replacing b's trees released %v (%v), want shared, which nothing names now", released, err)
	}
}

func assertCacheEntry(t *testing.T, st store.Store, kind store.CacheKind, key string, want map[string]store.CachedTree) {
	t.Helper()

	got, err := st.CacheEntry(ctxFor(t), kind, key)
	if err != nil {
		t.Fatalf("CacheEntry(%s, %q): %v", kind, key, err)
	}

	if !maps.Equal(got, want) {
		t.Fatalf("CacheEntry(%s, %q) = %v, want %v", kind, key, got, want)
	}
}
