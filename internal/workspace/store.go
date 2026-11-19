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
}

func newTreeStore(backend treeBackend, root string) (*treeStore, error) {
	dir := filepath.Join(root, storeDirName)

	err := os.MkdirAll(dir, 0o750)
	if err != nil {
		return nil, fmt.Errorf("could not create the store %q: %w", dir, err)
	}

	return &treeStore{backend: backend, dir: dir, grace: defaultStoreGrace}, nil
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

	return digest, s.install(staged, digest)
}

// install renames a staged tree into place under digest; one already there wins, since equal digests are equal bytes.
func (s *treeStore) install(staged, digest string) error {
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
	}

	s.touch(path)

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
		discardTree(path, s.backend.remove)

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
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}

		slog.Info("workspace.store_evict", "tree", path)

		err = s.backend.remove(path)
		if err != nil {
			slog.Warn("workspace.store_evict_failed", "tree", path, "error", err)
		}
	}
}
