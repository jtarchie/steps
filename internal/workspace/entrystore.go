package workspace

// The mechanics both on-disk caches under a workspace root share: content-keyed
// entry directories, and least-recently-used eviction.

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// entryStore owns one cache directory: where a key's entry lives, and which
// entries to evict when there are too many.
type entryStore struct {
	dir        string
	maxEntries int
	// removeEntry tears down a single entry. The two caches store
	// differently shaped ones — a resource version is a single materialized
	// tree, a step's outputs are a plain directory holding one tree per
	// output — and on btrfs the call that removes a subvolume is not the call
	// that removes a plain directory containing subvolumes.
	removeEntry func(dir string) error
}

// newEntryStore prepares a cache directory under root. A failure to create it
// is returned: the caller decides whether to run without a cache, and silently
// doing so would turn a misconfigured root into a mysteriously slow pipeline.
func newEntryStore(root, name string, maxEntries int, removeEntry func(dir string) error) (*entryStore, error) {
	dir := filepath.Join(root, name)

	err := os.MkdirAll(dir, 0o750)
	if err != nil {
		return nil, fmt.Errorf("could not create cache %q: %w", dir, err)
	}

	return &entryStore{dir: dir, maxEntries: maxEntries, removeEntry: removeEntry}, nil
}

// path is where a key's entry lives, and whether the key was usable at all.
// Keys are hex hashes, so they need no sanitizing — but this asserts it rather
// than assuming, since a key that could contain a separator would let a
// poisoned entry escape the cache directory.
func (s *entryStore) path(key string) (string, bool) {
	if key == "" || strings.ContainsAny(key, `/\.`) {
		return "", false
	}

	return filepath.Join(s.dir, key), true
}

// touch records an entry as recently used, so prune's least-recently-used
// ordering reflects reads and not only writes.
func (s *entryStore) touch(path string) {
	now := time.Now()

	err := os.Chtimes(path, now, now)
	if err != nil {
		slog.Debug("workspace.cache_touch_failed", "entry", path, "error", err)
	}
}

// prune evicts least-recently-used entries down to maxEntries.
//
// Recency is the directory's mtime, refreshed by touch on every hit. That is
// coarse — a filesystem may not update it the way a real LRU would — but the
// alternative is a metadata file to keep consistent with the directories it
// describes, and getting that wrong loses cache entries rather than merely
// evicting the wrong one.
func (s *entryStore) prune() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		slog.Debug("workspace.cache_prune_skipped", "dir", s.dir, "error", err)

		return
	}

	type aged struct {
		name string
		mod  time.Time
	}

	dirs := make([]aged, 0, len(entries))

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}

		dirs = append(dirs, aged{name: entry.Name(), mod: info.ModTime()})
	}

	if len(dirs) <= s.maxEntries {
		return
	}

	sort.Slice(dirs, func(i, j int) bool { return dirs[i].mod.Before(dirs[j].mod) })

	for _, victim := range dirs[:len(dirs)-s.maxEntries] {
		path := filepath.Join(s.dir, victim.name)

		slog.Info("workspace.cache_evict", "entry", path)

		removeErr := s.removeEntry(path)
		if removeErr != nil {
			slog.Warn("workspace.cache_evict_failed", "entry", path, "error", removeErr)
		}
	}
}
