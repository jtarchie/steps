package workspace

// The host's content-addressed store (#259 phase 2): every tree a cache keeps on this machine, filed once under its digest. The caches hold pointers at it, so two steps that produced the same bytes cost one tree.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// storeDirName is the store's directory under a durable root; the stale-build sweep leaves it alone because it is not a buildDirPrefix name.
const storeDirName = "store"

// defaultStoreGrace is how long a tree filed or used is safe from release: another steps process sharing the root may be between filing it and indexing it, which this one cannot see.
const defaultStoreGrace = 10 * time.Minute

var errStoreTreeGone = errors.New("the store no longer has this tree")

type treeStore struct {
	backend treeBackend
	dir     string
	grace   time.Duration
	// maxBytes bounds the trees' file bytes, least recently used evicted first; zero is unbounded.
	maxBytes int64
}

func newTreeStore(backend treeBackend, root string, maxBytes int64) (*treeStore, error) {
	dir := filepath.Join(root, storeDirName)

	err := os.MkdirAll(dir, 0o750)
	if err != nil {
		return nil, fmt.Errorf("could not create the store %q: %w", dir, err)
	}

	return &treeStore{backend: backend, dir: dir, grace: defaultStoreGrace, maxBytes: maxBytes}, nil
}

// path is where digest's tree lives, refusing anything that is not a bare hex digest so a record cannot name a path outside the store.
func (s *treeStore) path(digest string) (string, error) {
	if digest == "" || strings.Trim(digest, "0123456789abcdef") != "" {
		return "", fmt.Errorf("not a tree digest: %q", digest)
	}

	return filepath.Join(s.dir, digest), nil
}

func (s *treeStore) has(digest string) bool {
	path, err := s.path(digest)
	if err != nil {
		return false
	}

	_, err = os.Stat(path)

	return err == nil
}

// put files src's bytes and returns their digest. The digest is of the clone, not of src, so it names what the store holds even if src changes under the copy; a digest already filed costs only the clone.
func (s *treeStore) put(ctx context.Context, src string) (string, error) {
	err := rejectSymlinkSrc(src)
	if err != nil {
		return "", err
	}

	staged, err := stageTree(filepath.Join(s.dir, ".put"), s.backend.remove, func(staged string) error {
		return s.backend.materialize(ctx, src, staged)
	})
	if err != nil {
		return "", fmt.Errorf("filing %q in the store: %w", src, err)
	}

	digest, err := digestTree(staged)
	if err != nil {
		discardTree(staged, s.backend.remove)

		return "", err
	}

	return digest, s.install(ctx, staged, digest)
}

// install renames a staged tree into place under digest; one already there wins, since equal digests are equal bytes.
func (s *treeStore) install(ctx context.Context, staged, digest string) error {
	path, err := s.path(digest)
	if err != nil {
		discardTree(staged, s.backend.remove)

		return err
	}

	err = os.Rename(staged, path)
	if err != nil {
		discardTree(staged, s.backend.remove)

		if !s.has(digest) {
			return fmt.Errorf("filing %s in the store: %w", digest, err)
		}

		s.touch(path)

		return nil
	}

	s.touch(path)
	s.recordSize(digest)
	s.bound(ctx, digest)

	return nil
}

// materialize copies digest's tree into dst, a fresh path, and proves the copy is those bytes. A copy that is not is removed with the store's tree, which is corrupt, and is reported as errStoreTreeGone: a miss, never a wrong tree handed to a step (#119).
func (s *treeStore) materialize(ctx context.Context, digest, dst string) error {
	path, err := s.path(digest)
	if err != nil {
		return err
	}

	_, err = os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return errStoreTreeGone
	}

	err = rejectSymlinkSrc(path)
	if err != nil {
		return err
	}

	err = s.backend.materialize(ctx, path, dst)
	if err != nil {
		return fmt.Errorf("materializing %s from the store: %w", digest, err)
	}

	got, err := digestTree(dst)
	if err != nil || got != digest {
		slog.WarnContext(ctx, "workspace.store_tree_corrupt", "digest", digest, "got", got, "error", err)
		discardTree(dst, s.backend.remove)
		s.remove(digest)

		return errStoreTreeGone
	}

	s.touch(path)

	return nil
}

func (s *treeStore) touch(path string) {
	now := time.Now()

	err := os.Chtimes(path, now, now)
	if err != nil {
		slog.Debug("workspace.store_touch_failed", "tree", path, "error", err)
	}
}

// release removes the trees an index stopped naming, sparing one touched within the grace: another state database sharing this root may have just filed or used the same bytes.
func (s *treeStore) release(digests []string) {
	cutoff := time.Now().Add(-s.grace)

	for _, digest := range digests {
		path, err := s.path(digest)
		if err != nil {
			continue
		}

		info, err := os.Stat(path)
		if err != nil || info.ModTime().After(cutoff) || s.pinned(digest) {
			continue
		}

		slog.Info("workspace.store_evict", "tree", path, "reason", "no entry names it")
		s.remove(digest)
	}
}

// sizesDirName holds each tree's byte count, written when it is filed, so bounding the store never walks every tree.
const sizesDirName = ".sizes"

// DefaultStoreMaxBytes bounds the store when the operator does not say: the worker cache's bound, so a tree costs the same headroom on this disk as in docker.
const DefaultStoreMaxBytes = 8 << 30

// bound evicts least recently used trees until the store fits maxBytes, never the one just filed: an entry whose tree went is a miss, and its step runs.
func (s *treeStore) bound(ctx context.Context, filed string) {
	if s.maxBytes <= 0 {
		return
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		slog.DebugContext(ctx, "workspace.store_bound_skipped", "error", err)

		return
	}

	type tree struct {
		digest string
		used   time.Time
		bytes  int64
	}

	trees := make([]tree, 0, len(entries))

	var total int64

	for _, entry := range entries {
		info, infoErr := entry.Info()
		if strings.HasPrefix(entry.Name(), ".") || infoErr != nil || !info.IsDir() {
			continue
		}

		bytes := s.size(entry.Name())
		total += bytes
		trees = append(trees, tree{digest: entry.Name(), used: info.ModTime(), bytes: bytes})
	}

	slices.SortFunc(trees, func(a, b tree) int { return a.used.Compare(b.used) })

	for _, victim := range trees {
		if total <= s.maxBytes {
			return
		}

		if !s.evictable(victim.digest, filed) {
			continue
		}

		slog.InfoContext(ctx, "workspace.store_evict", "digest", victim.digest, "bytes", victim.bytes, "reason", "store over --store-max-bytes")
		s.remove(victim.digest)

		total -= victim.bytes
	}
}

// evictable is what bound may take: never the tree just filed, nor one a failed run is waiting on.
func (s *treeStore) evictable(digest, filed string) bool {
	return digest != filed && !s.pinned(digest)
}

// size is a tree's recorded byte count, measured and recorded when the record is missing.
func (s *treeStore) size(digest string) int64 {
	data, err := os.ReadFile(filepath.Join(s.dir, sizesDirName, digest)) //nolint:gosec // a digest listed from the store's own directory
	if err == nil {
		bytes, parseErr := strconv.ParseInt(string(data), 10, 64)
		if parseErr == nil {
			return bytes
		}
	}

	return s.recordSize(digest)
}

// recordSize measures a tree's file bytes and records them beside the store.
func (s *treeStore) recordSize(digest string) int64 {
	var bytes int64

	_ = filepath.WalkDir(filepath.Join(s.dir, digest), func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a size is an estimate; an unreadable entry counts nothing
		}

		info, infoErr := entry.Info()
		if infoErr == nil && info.Mode().IsRegular() {
			bytes += info.Size()
		}

		return nil
	})

	err := os.MkdirAll(filepath.Join(s.dir, sizesDirName), 0o750)
	if err == nil {
		err = os.WriteFile(filepath.Join(s.dir, sizesDirName, digest), []byte(strconv.FormatInt(bytes, 10)), 0o600)
	}

	if err != nil {
		slog.Debug("workspace.store_size_unrecorded", "digest", digest, "error", err)
	}

	return bytes
}

func (s *treeStore) remove(digest string) {
	err := s.backend.remove(filepath.Join(s.dir, digest))
	if err != nil {
		slog.Warn("workspace.store_evict_failed", "digest", digest, "error", err)

		return
	}

	_ = os.Remove(filepath.Join(s.dir, sizesDirName, digest))
}
