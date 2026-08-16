package workspace

// The cross-build resource cache: keeping a fetched resource version so the
// next build snapshots it instead of fetching it again.

import (
	"context"
	"log/slog"
	"os"
)

// cacheDirName is the directory under an isolating root that holds cached
// resource versions. The buildDirPrefix sweep ignores it by name, which is the
// point: build directories are garbage after a crash, cache entries are the
// asset.
const cacheDirName = "cache"

// resourceCache stores fetched resource versions under root/cache/<key>, keyed
// by the content hash of everything that determines what a fetch produces.
//
// The win it exists for is the one baggageclaim provides in Concourse. Within
// a build, btrfs already makes materialization free: a get lands in an
// artifact subvolume and every step snapshots from it. Across builds there was
// nothing — a real run re-fetched every get from scratch, paying the network
// and the disk again each time. With this, the second build snapshots a
// version the first one fetched.
//
// Entries are whole directories materialized through the provider's own
// treeBackend, so on btrfs a hit costs a subvolume snapshot (instant, no
// copied bytes) and on copy it costs a copy but still no fetch.
type resourceCache struct {
	backend treeBackend
	entries *entryStore
}

// newResourceCache prepares the cache directory.
func newResourceCache(backend treeBackend, root string, maxEntries int) (*resourceCache, error) {
	entries, err := newEntryStore(root, cacheDirName, maxEntries, backend.remove)
	if err != nil {
		return nil, err
	}

	return &resourceCache{backend: backend, entries: entries}, nil
}

// Fetch materializes the version identified by key into dst.
//
// On a hit it snapshots the cached entry. On a miss it calls fetch to populate
// dst for real, then seeds the cache from the result — in that order, so a
// failed fetch leaves nothing cached and the next build tries again rather
// than reusing a half-written tree.
//
// Everything about the cache is best-effort except the fetch itself: if the
// cache cannot be read, written, or pruned, the fetch still happens and the
// build still proceeds. A cache is an optimization, and an optimization that
// can fail a build is a liability.
func (c *resourceCache) Fetch(ctx context.Context, key, dst string, fetch func() error) error {
	path, ok := c.entries.path(key)
	if !ok {
		slog.Warn("workspace.cache_bad_key", "key", key)

		return fetch()
	}

	if c.restore(ctx, path, dst) {
		return nil
	}

	err := fetch()
	if err != nil {
		return err
	}

	c.store(ctx, path, dst)
	c.entries.prune()

	return nil
}

// restore replaces dst with a copy of the cached entry, reporting whether it
// managed to. dst already exists (the caller created it), and materialize
// requires its destination not to — so the empty directory is removed first.
// If anything goes wrong the caller must still be able to fetch into dst, so
// a failed restore recreates it empty.
func (c *resourceCache) restore(ctx context.Context, path, dst string) bool {
	_, err := os.Stat(path)
	if err != nil {
		return false
	}

	// Same guard materializeSpace and Capture apply, for the same reason: the
	// copy backend's `cp -R -P -p src/. dst` dereferences a symlink AT src
	// despite -P, and a step's shell commands can reach this directory by
	// absolute path (isolation is hygiene, not a sandbox). A cache entry
	// swapped for a symlink would otherwise copy that target into the next
	// build's resource directory.
	err = rejectSymlinkSrc(path)
	if err != nil {
		slog.Warn("workspace.cache_entry_rejected", "entry", path, "error", err)

		return false
	}

	err = c.backend.remove(dst)
	if err != nil {
		slog.Warn("workspace.cache_restore_failed", "entry", path, "error", err)

		return false
	}

	err = c.backend.materialize(ctx, path, dst)
	if err != nil {
		slog.Warn("workspace.cache_restore_failed", "entry", path, "error", err)

		// dst is gone and the snapshot did not land. Put an empty directory
		// back so the caller's fetch has somewhere to write.
		createErr := c.backend.createEmpty(ctx, dst)
		if createErr != nil {
			slog.Error("workspace.cache_restore_recovery_failed", "dir", dst, "error", createErr)
		}

		return false
	}

	// Touch on use: prune evicts least-recently-used, and a version fetched
	// once and reused daily must not be evicted for being old.
	c.entries.touch(path)

	slog.Debug("workspace.cache_hit", "entry", path)

	return true
}

// store seeds the cache from a freshly fetched directory.
func (c *resourceCache) store(ctx context.Context, path, src string) {
	// The mirror of restore's guard: an in: that replaced its own output
	// directory with a symlink would otherwise have that target's contents
	// copied into the cache, and served to every later build.
	err := rejectSymlinkSrc(src)
	if err != nil {
		slog.Warn("workspace.cache_store_rejected", "src", src, "error", err)

		return
	}

	err = c.backend.remove(path)
	if err != nil {
		slog.Warn("workspace.cache_store_failed", "entry", path, "error", err)

		return
	}

	err = c.backend.materialize(ctx, src, path)
	if err != nil {
		slog.Warn("workspace.cache_store_failed", "entry", path, "error", err)

		return
	}

	slog.Debug("workspace.cache_store", "entry", path)
}
