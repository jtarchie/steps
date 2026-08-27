package shim

// What a worker already holds.
//
// A step's tree arrives as one entry per artifact, each named by the digest
// of its content, so a worker can place what it already has instead of
// pulling it down again. That is the whole saving: two steps of one job share
// their inputs and differ only in their outputs, and measured on a real job a
// 64MB input through three placed steps moved 192MB — the store deduplicating
// none of it, because whole trees differ by an empty directory and hash
// differently.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// artifactCacheBytes bounds what the cache may hold on a worker.
//
// Bounded by SIZE and not by age, for the reason CLAUDE.md gives about the
// merkle cache: a worker used every day would have a busy cache swept out
// from under it by an age floor, and the faster it worked the sooner it lost
// what it had. A cap only evicts when something actually needs the room.
//
// A worker is somebody else's machine, and filling its disk is a worse
// failure than re-fetching a tree.
const artifactCacheBytes = 8 << 30

// artifactCacheDir is where this shim keeps artifacts between sessions.
//
// Beside the session scratch and not inside it: the scratch is removed when
// its session ends, and a cache that died with the step that filled it would
// never be reused by anything.
func (s *session) artifactCacheDir() string {
	return filepath.Join(filepath.Dir(filepath.Dir(s.workdir)), "artifacts")
}

// sweepArtifactCache brings the cache back under its cap, coldest first.
//
// Best effort: a cache that cannot be swept is a disk problem, and failing
// the step over it would turn an optimization into an outage.
func sweepArtifactCache(cache string) error {
	entries, err := os.ReadDir(cache)
	if err != nil {
		return nil //nolint:nilerr // no cache is not a failure; the next fetch makes one
	}

	type held struct {
		path  string
		used  time.Time
		bytes int64
	}

	kept := make([]held, 0, len(entries))
	total := int64(0)

	for _, entry := range entries {
		info, statErr := entry.Info()
		if statErr != nil {
			continue
		}

		path := filepath.Join(cache, entry.Name())
		size := treeBytes(path)
		total += size

		kept = append(kept, held{path: path, used: info.ModTime(), bytes: size})
	}

	if total <= artifactCacheBytes {
		return nil
	}

	sort.Slice(kept, func(i, j int) bool { return kept[i].used.Before(kept[j].used) })

	for _, entry := range kept {
		if total <= artifactCacheBytes {
			break
		}

		_ = os.RemoveAll(entry.path)
		total -= entry.bytes
	}

	return nil
}

// treeBytes is what one cached artifact occupies, best effort.
func treeBytes(root string) int64 {
	var total int64

	_ = filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil //nolint:nilerr // a file that vanished mid-walk is not this function's problem
		}

		info, statErr := entry.Info()
		if statErr == nil {
			total += info.Size()
		}

		return nil
	})

	return total
}

// copyTree materializes a cached artifact into the work directory.
//
// A copy, not a link: the step is about to write into its own tree, and a
// hard link would edit the cached copy every later step depends on. On a
// filesystem with reflinks this is the obvious place to make it free, which
// is a further optimization rather than a correctness matter — the bytes that
// cost are the ones that crossed the network, and those are already saved.
func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("reading a cached artifact: %w", err)
	}

	switch {
	case info.IsDir():
		return copyDir(src, dst, info)
	case info.Mode()&os.ModeSymlink != 0:
		target, readErr := os.Readlink(src)
		if readErr != nil {
			return fmt.Errorf("reading a cached link: %w", readErr)
		}

		_ = os.Remove(dst)

		return os.Symlink(target, dst) //nolint:wrapcheck // the path is in the message
	default:
		return copyFile(src, dst, info)
	}
}

func copyDir(src, dst string, info os.FileInfo) error {
	err := os.MkdirAll(dst, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("making %q: %w", dst, err)
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("reading %q: %w", src, err)
	}

	for _, entry := range entries {
		err = copyTree(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name()))
		if err != nil {
			return err
		}
	}

	return nil
}

func copyFile(src, dst string, info os.FileInfo) error {
	from, err := os.Open(src) //nolint:gosec // src is this shim's own cache entry, not a peer's path
	if err != nil {
		return fmt.Errorf("reading a cached artifact: %w", err)
	}

	defer func() { _ = from.Close() }()

	//nolint:gosec // dst is under this session's work directory, which checkSessionName bounds
	to, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("writing %q: %w", dst, err)
	}

	_, err = io.Copy(to, from)
	if err != nil {
		_ = to.Close()

		return fmt.Errorf("writing %q: %w", dst, err)
	}

	// Closed explicitly: a delayed-allocation filesystem reports ENOSPC here
	// and nowhere else, and a truncated artifact would be cached as complete.
	err = to.Close()
	if err != nil {
		return fmt.Errorf("writing %q: %w", dst, err)
	}

	return nil
}

// stageArtifact makes the directory an incoming artifact is unpacked into
// before it is named by its digest.
func stageArtifact(cache string) (string, error) {
	err := os.MkdirAll(cache, 0o700)
	if err != nil {
		return "", fmt.Errorf("making the artifact cache: %w", err)
	}

	staging, err := os.MkdirTemp(cache, ".partial-*")
	if err != nil {
		return "", fmt.Errorf("staging an artifact: %w", err)
	}

	return staging, nil
}

// commitArtifact names a fully-unpacked artifact by its digest.
//
// A racing session completing the same digest first is the cache working, not
// a failure — both were about to hold identical content.
func commitArtifact(staging, held string) error {
	err := os.Rename(staging, held)
	if err == nil {
		return nil
	}

	_, statErr := os.Stat(held)
	if statErr != nil {
		return fmt.Errorf("placing an artifact in the cache: %w", err)
	}

	return nil
}

// placeHeldArtifact copies one cached artifact into a work directory, and
// marks it used so the sweep evicts what is coldest rather than what is
// merely oldest.
func placeHeldArtifact(held, name, workdir string) error {
	now := time.Now()
	_ = os.Chtimes(held, now, now)

	return copyTree(filepath.Join(held, name), filepath.Join(workdir, name))
}
