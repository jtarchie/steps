package workspace

// Resume from the store (#259 phase 4): a failed build files each artifact into the store beside its tree and keeps only the record of the digest, so a failed run costs the store's bounded bytes rather than a tree nobody reclaims; a resume clones its inputs back from those digests. A record with no holder in held.json names this store, as it does in the cache index.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// pinsDirName holds, per digest, one marker per failed build tree still waiting on it; the store's bound and release spare a pinned tree.
const pinsDirName = ".pins"

// shelfPinLifetime is how long a failed run's trees are safe from eviction: the lifetime a docker worker pins its held outputs for, so a resume has one window whichever holder kept its files.
const shelfPinLifetime = 48 * time.Hour

// Shelve files every artifact the build holds into the store, records each as held there, and removes the tree's copies. Not under --keep-workspace, which keeps the files to be read, nor without a store.
func (b *isolatingBuild) Shelve(ctx context.Context) error {
	if b.keepFiles || b.shelf == nil {
		return nil
	}

	entries, err := os.ReadDir(b.artifacts)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("filing %s in the store: %w", b.artifacts, err)
	}

	owner := filepath.Base(b.root)

	b.remoteMu.Lock()
	defer b.remoteMu.Unlock()

	if b.remote == nil {
		b.remote = map[string]RemoteArtifact{}
	}

	// Pinned before anything is filed, so filing one tree never evicts another the resume needs.
	for _, held := range b.remote {
		if held.Holder == "" {
			b.shelf.pin(held.Digest, owner)
		}
	}

	var shelveErr error

	for _, entry := range entries {
		shelveErr = errors.Join(shelveErr, b.shelveEntry(ctx, entry, owner))
	}

	// Whatever was filed is recorded even when another artifact failed: it is no longer in the tree, and the record is the only way back to it.
	err = b.saveHeld()
	if err != nil {
		return errors.Join(shelveErr, err)
	}

	discardTree(b.stepsDir, b.backend.remove)

	return shelveErr
}

// shelveEntry files one entry of artifacts/ and records it, with remoteMu held; a staging leftover is garbage and goes.
func (b *isolatingBuild) shelveEntry(ctx context.Context, entry fs.DirEntry, owner string) error {
	src := filepath.Join(b.artifacts, entry.Name())

	if !entry.IsDir() || strings.Contains(entry.Name(), stagedMarker) {
		discardTree(src, b.backend.remove)

		return nil
	}

	digest, err := b.shelf.adopt(ctx, src)
	if err != nil {
		return fmt.Errorf("filing artifact %q in the store: %w", entry.Name(), err)
	}

	b.shelf.pin(digest, owner)
	b.remote[entry.Name()] = RemoteArtifact{Digest: digest, Pull: b.shelf.pull(digest)}

	return nil
}

// shelfFor is the store a tree's artifacts are filed in: the one beside it, which is this provider's for a tree under its root and may be another root's for a resumed run's.
func (p *isolatingProvider) shelfFor(tree string) *treeStore {
	if p.store != nil && absDir(filepath.Dir(tree)) == absDir(p.root) {
		return p.store
	}

	dir := filepath.Join(filepath.Dir(tree), storeDirName)

	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}

	maxBytes := int64(DefaultStoreMaxBytes)
	if p.store != nil {
		maxBytes = p.store.maxBytes
	}

	return &treeStore{backend: p.backend, dir: dir, grace: defaultStoreGrace, maxBytes: maxBytes}
}

// ShelvedGone names an artifact a failed run filed in the store beside root that the store no longer has, or "" when it has every one: what a resume checks before it puts the run back in flight.
func ShelvedGone(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, heldFile)) //nolint:gosec // the tree a run recorded as its own
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("reading what %s holds: %w", root, err)
	}

	var records map[string]heldRecord

	err = json.Unmarshal(data, &records)
	if err != nil {
		return "", fmt.Errorf("reading what %s holds: %w", root, err)
	}

	store := &treeStore{dir: filepath.Join(filepath.Dir(root), storeDirName)}

	for name, record := range records {
		if record.Holder != "" || shelvedRecordStale(root, name) {
			continue
		}

		if !store.has(record.Digest) {
			return name, nil
		}
	}

	return "", nil
}

// shelvedRecordStale is staleHold for a tree no build has been opened on: an artifact back on disk was captured or fetched since, and replaces the record.
func shelvedRecordStale(root, name string) bool {
	_, err := os.Lstat(filepath.Join(root, "artifacts", name))

	return err == nil
}

// adopt files src by moving it in rather than copying it, for a caller that is done with src; one with a digest already filed is dropped.
func (s *treeStore) adopt(ctx context.Context, src string) (string, error) {
	err := rejectSymlinkSrc(src)
	if err != nil {
		return "", err
	}

	digest, err := digestTree(src)
	if err != nil {
		return "", err
	}

	if s.has(digest) {
		path, _ := s.path(digest)
		s.touch(path)

		return digest, s.backend.remove(src)
	}

	path, err := s.path(digest)
	if err != nil {
		return "", err
	}

	err = os.Rename(src, path)
	if err == nil {
		s.touch(path)
		s.recordSize(digest)
		s.bound(ctx, digest)

		return digest, nil
	}

	if s.has(digest) {
		return digest, s.backend.remove(src)
	}

	// Across filesystems a rename cannot move it, and a copy can; src stays until the copy is filed, so a failure loses nothing.
	filed, err := s.put(ctx, src)
	if err != nil {
		return "", err
	}

	return filed, s.backend.remove(src)
}

// pull is a RemoteArtifact.Pull from this store: dst, an existing empty directory, is replaced by a proven copy of digest's tree.
func (s *treeStore) pull(digest string) func(context.Context, string) error {
	return func(ctx context.Context, dst string) error {
		err := s.backend.remove(dst)
		if err != nil {
			return fmt.Errorf("replacing %s: %w", dst, err)
		}

		return s.materialize(ctx, digest, dst)
	}
}

func (s *treeStore) pinPath(digest, owner string) (string, error) {
	_, err := s.path(digest)
	if err != nil {
		return "", err
	}

	return filepath.Join(s.dir, pinsDirName, digest, owner), nil
}

// pin keeps digest's tree for owner, a failed build tree, for shelfPinLifetime from now.
func (s *treeStore) pin(digest, owner string) {
	path, err := s.pinPath(digest, owner)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o750)
	}

	if err == nil {
		err = os.WriteFile(path, nil, 0o600)
	}

	if err == nil {
		now := time.Now()
		err = os.Chtimes(path, now, now)
	}

	if err != nil {
		slog.Warn("workspace.store_pin_failed", "digest", digest, "error", err)
	}
}

// pinned reports whether a failed build still waits on digest's tree, removing pins past their lifetime.
func (s *treeStore) pinned(digest string) bool {
	dir := filepath.Join(s.dir, pinsDirName, digest)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	cutoff := time.Now().Add(-shelfPinLifetime)
	live := false

	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr == nil && info.ModTime().After(cutoff) {
			live = true

			continue
		}

		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}

	if !live {
		_ = os.Remove(dir)
	}

	return live
}

// unpin drops every pin owner holds, once its run no longer needs the trees.
func (s *treeStore) unpin(owner string) {
	pins, err := filepath.Glob(filepath.Join(s.dir, pinsDirName, "*", owner))
	if err != nil {
		return
	}

	for _, pin := range pins {
		_ = os.Remove(pin)
		_ = os.Remove(filepath.Dir(pin))
	}
}
