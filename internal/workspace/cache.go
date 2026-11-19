package workspace

// The cross-build resource cache: a fetched version is kept so the next build need not fetch it again.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
)

// cacheDirName is not a buildDirPrefix name, so the stale-build sweep spares it: build directories are garbage after a crash, cache entries are the asset.
const cacheDirName = "cache"

// resourceCache keeps one pointer per fetched version, keyed by everything that determines what a fetch produces, at a tree in the store or in docker (heldcache.go): baggageclaim's resource caches, so a second build need not pay the network again.
type resourceCache struct {
	backend treeBackend
	store   *treeStore
	entries *entryStore
}

func newResourceCache(backend treeBackend, store *treeStore, root string, maxEntries int) (*resourceCache, error) {
	entries, err := newEntryStore(root, cacheDirName, maxEntries, backend.remove)
	if err != nil {
		return nil, err
	}

	return &resourceCache{backend: backend, store: store, entries: entries}, nil
}

// Fetch files only after fetch succeeded, so a failed fetch caches nothing; everything but the fetch is best-effort, since an optimization that can fail a build is a liability.
func (c *resourceCache) Fetch(ctx context.Context, key, dst string, fetch func() error) error {
	path, ok := c.entries.path(key)
	if !ok {
		slog.WarnContext(ctx, "workspace.cache_bad_key", "key", key)

		return fetch()
	}

	if c.restore(ctx, path, dst) {
		return nil
	}

	err := fetch()
	if errors.Is(err, errFiledHeld) {
		c.prune()

		return nil
	}

	if err != nil {
		return err
	}

	c.file(ctx, path, dst)
	c.prune()

	return nil
}

func (c *resourceCache) prune() {
	c.entries.prune()
	c.store.collect()
}

// restore replaces dst with a copy of the stored tree the entry points at, reporting whether it managed to. Staged beside dst, so a failed restore leaves the empty directory the caller created for its fetch exactly as it was.
func (c *resourceCache) restore(ctx context.Context, path, dst string) bool {
	records, err := readHeld(path)
	if err != nil {
		slog.WarnContext(ctx, "workspace.cache_entry_rejected", "entry", path, "error", err)

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
		slog.WarnContext(ctx, "workspace.cache_restore_failed", "entry", path, "error", err)

		return false
	}

	// A version fetched once and reused daily must not be evicted for being old.
	c.entries.touch(path)

	slog.DebugContext(ctx, "workspace.cache_hit", "entry", path)

	return true
}

// file puts a freshly fetched directory in the store and points the entry at it.
func (c *resourceCache) file(ctx context.Context, path, src string) {
	digest, err := c.store.put(ctx, src)
	if err != nil {
		slog.WarnContext(ctx, "workspace.cache_store_failed", "entry", path, "error", err)

		return
	}

	c.point(ctx, path, heldRecord{Digest: digest})
}

// point files a resource entry as a pointer at record's tree.
func (c *resourceCache) point(ctx context.Context, path string, record heldRecord) {
	// Staged and renamed into place: restore takes an entry's existence as the whole of "cached", so one filled in place and interrupted would be served to every later build.
	err := replaceTree(path, c.backend.remove, func(staged string) error {
		mkErr := os.MkdirAll(staged, 0o750)
		if mkErr != nil {
			return fmt.Errorf("creating cache entry: %w", mkErr)
		}

		return writeHeld(staged, map[string]heldRecord{resourceHeldKey: record})
	})
	if err != nil {
		slog.WarnContext(ctx, "workspace.cache_store_failed", "entry", path, "error", err)

		return
	}

	slog.DebugContext(ctx, "workspace.cache_store", "entry", path, "held", record.Holder != "")
}
