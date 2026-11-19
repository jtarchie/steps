package workspace

// The cross-build resource cache: a fetched version is kept so the next build need not fetch it again.

import (
	"context"
	"errors"
	"log/slog"
)

// resourceCache indexes one tree per fetched version, keyed by everything that determines what a fetch produces, in the store or in docker (heldcache.go): baggageclaim's resource caches, so a second build need not pay the network again.
type resourceCache struct {
	backend    treeBackend
	store      *treeStore
	index      CacheIndex
	maxEntries int
}

// Fetch files only after fetch succeeded, so a failed fetch caches nothing; everything but the fetch is best-effort, since an optimization that can fail a build is a liability.
func (c *resourceCache) Fetch(ctx context.Context, key, dst string, fetch func() error) error {
	if c.restore(ctx, key, dst) {
		return nil
	}

	err := fetch()
	if errors.Is(err, errFiledHeld) {
		return nil
	}

	if err != nil {
		return err
	}

	c.file(ctx, key, dst)

	return nil
}

// restore replaces dst with a copy of the stored tree the entry names, reporting whether it managed to. Staged beside dst, so a failed restore leaves the empty directory the caller created for its fetch exactly as it was.
func (c *resourceCache) restore(ctx context.Context, key, dst string) bool {
	records, err := c.index.CacheEntry(ctx, kindResource, key)
	if err != nil {
		slog.WarnContext(ctx, "workspace.cache_lookup_failed", "key", key, "error", err)

		return false
	}

	// A tree docker holds is fetchHeld's to answer.
	record, ok := records[resourceHeldKey]
	if !ok || record.Holder != "" {
		return false
	}

	err = replaceTree(dst, c.backend.remove, func(staged string) error {
		return c.store.materialize(ctx, record.Digest, staged)
	})
	if err != nil {
		slog.WarnContext(ctx, "workspace.cache_restore_failed", "key", key, "error", err)

		return false
	}

	c.touch(ctx, key)

	slog.DebugContext(ctx, "workspace.cache_hit", "key", key)

	return true
}

// touch keeps a version fetched once and reused daily from being evicted for being old.
func (c *resourceCache) touch(ctx context.Context, key string) {
	err := c.index.TouchCacheEntry(ctx, kindResource, key)
	if err != nil {
		slog.DebugContext(ctx, "workspace.cache_touch_failed", "key", key, "error", err)
	}
}

// file puts a freshly fetched directory in the store and points the entry at it.
func (c *resourceCache) file(ctx context.Context, key, src string) {
	digest, err := c.store.put(ctx, src)
	if err != nil {
		slog.WarnContext(ctx, "workspace.cache_store_failed", "key", key, "error", err)

		return
	}

	c.point(ctx, key, CachedTree{Digest: digest})
}

// point indexes a resource entry as record's tree, then removes the trees it and the count bound released.
func (c *resourceCache) point(ctx context.Context, key string, record CachedTree) {
	released, err := c.index.RecordCacheEntry(ctx, kindResource, key, map[string]CachedTree{resourceHeldKey: record}, c.maxEntries)
	if err != nil {
		slog.WarnContext(ctx, "workspace.cache_store_failed", "key", key, "error", err)

		return
	}

	c.store.release(released)

	slog.DebugContext(ctx, "workspace.cache_store", "key", key, "held", record.Holder != "")
}
