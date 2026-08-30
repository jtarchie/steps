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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/compress"
	"github.com/jtarchie/steps/internal/wire"
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
//
// A variable rather than a constant so a test can shrink it: what is worth
// proving is WHAT the sweep counts and what it frees, not that it can be made
// to move eight gigabytes.
//
//nolint:gochecknoglobals // a test seam over one bound, not state
var artifactCacheBytes int64 = 8 << 30

// artifactCacheName is the cache's directory name under <root>/steps-shim,
// beside the per-session scratch directories rather than inside one.
//
// Named once and reserved by checkSessionName from the same constant: a
// session called this IS the shared cache, so the sweep walks a live work
// directory as a cache entry and cleanup's RemoveAll takes every other
// session's cache with it.
const artifactCacheName = "artifacts"

// artifactCacheDir is where this shim keeps artifacts between sessions.
//
// Beside the session scratch and not inside it: the scratch is removed when
// its session ends, and a cache that died with the step that filled it would
// never be reused by anything.
func (s *session) artifactCacheDir() string {
	return filepath.Join(filepath.Dir(filepath.Dir(s.workdir)), artifactCacheName)
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

	total += reclaimTombstones(cache, entries)

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), tombstonePrefix) {
			continue
		}

		// A staging directory is another session's transfer in flight, not a
		// cache entry: the cache is shared by every shim on the worker, and
		// evicting one of these deletes a tree mid-unpack — after which the
		// rename that follows commits a TRUNCATED tree under a digest that
		// claims to be whole, which is what staging exists to prevent.
		if strings.HasPrefix(entry.Name(), stagingPrefix) {
			// Counted even so. Skipping the eviction is the point; skipping
			// the ACCOUNTING made the cap measure something other than the
			// disk — a shim killed mid-unpack (OOM, spot reclamation, kill -9)
			// leaves a full-size staging tree its deferred cleanup never ran
			// on, and nothing else removes it, so the cache reported itself
			// under the cap while the directory held the cap plus every
			// orphan ever left there.
			total += treeBytes(filepath.Join(cache, entry.Name()))

			continue
		}

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

		// Only what was actually freed. Subtracting unconditionally let one
		// undeletable entry — a directory the codec restored read-only, which
		// RemoveAll cannot enter and does not chmod — end the loop having
		// freed nothing, every sweep, forever: it is the coldest entry, so it
		// is picked first each time, and the cap stops bounding anything.
		//
		// evictArtifact renames before it deletes, so a failure here has still
		// taken the entry out of service; what it leaves is a tombstone the
		// next sweep counts and retries, never a half-entry under a digest.
		if evictArtifact(entry.path) == nil {
			total -= entry.bytes
		}
	}

	return nil
}

// reclaimTombstones deletes what a previous sweep could not finish, and
// answers the bytes of whatever still refuses to go.
//
// Reclaimed before anything live is considered: a tombstone is already
// unreachable to every reader, so there is nothing to weigh it against.
// Counted when it survives, for the reason staging directories are — bytes
// the cap cannot see are bytes the cap does not bound.
func reclaimTombstones(cache string, entries []os.DirEntry) int64 {
	var stuck int64

	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), tombstonePrefix) {
			continue
		}

		tombstone := filepath.Join(cache, entry.Name())
		if os.RemoveAll(tombstone) != nil {
			stuck += treeBytes(tombstone)
		}
	}

	return stuck
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

// stagedDirMode is what a directory is created with while an artifact is
// being placed, before its recorded mode is applied. wire.UnpackTree's own
// constant, for the same two reasons: a recorded 0500 restored on the way
// past refuses its own children, and a directory created wide and narrowed
// later is briefly visible.
const stagedDirMode = 0o700

// copyTree materializes a cached artifact into the work directory.
//
// A copy, not a link: the step is about to write into its own tree, and a
// hard link would edit the cached copy every later step depends on. On a
// filesystem with reflinks this is the obvious place to make it free, which
// is a further optimization rather than a correctness matter — the bytes that
// cost are the ones that crossed the network, and those are already saved.
//
// Through os.Root, and never through plain path joins, for the reason
// wire.UnpackTree is: the cache holds trees a PEER authored, and this codec
// round-trips a symlink target verbatim — so an artifact can leave a link in
// the work directory that the next artifact of the same name would otherwise
// be written THROUGH. That is the write os.Root refuses, and it has to be
// refused on this path too or the protection only covers the archive and not
// the copy that stands in for it.
func copyTree(root *os.Root, src, rel string) error {
	modes := map[string]os.FileMode{}

	err := copyInto(root, src, rel, modes)
	if err != nil {
		return err
	}

	return applyDirModes(root, modes)
}

// copyInto copies one entry, recording the directory modes to restore once
// every child has been written.
func copyInto(root *os.Root, src, rel string, modes map[string]os.FileMode) error {
	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("reading a cached artifact: %w", err)
	}

	switch {
	case info.IsDir():
		return copyDir(root, src, rel, info, modes)
	case info.Mode()&os.ModeSymlink != 0:
		target, readErr := os.Readlink(src)
		if readErr != nil {
			return fmt.Errorf("reading a cached link: %w", readErr)
		}

		_ = root.Remove(rel)

		return root.Symlink(target, rel) //nolint:wrapcheck // the path is in the message
	default:
		return copyFile(root, src, rel, info)
	}
}

// applyDirModes restores recorded directory modes, deepest first so a
// directory narrowed by its own mode is not one this still has to write into.
func applyDirModes(root *os.Root, modes map[string]os.FileMode) error {
	names := make([]string, 0, len(modes))
	for name := range modes {
		names = append(names, name)
	}

	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	for _, name := range names {
		err := root.Chmod(name, modes[name])
		if err != nil {
			return fmt.Errorf("restoring the mode of %q: %w", name, err)
		}
	}

	return nil
}

func copyDir(root *os.Root, src, rel string, info os.FileInfo, modes map[string]os.FileMode) error {
	err := root.Mkdir(rel, stagedDirMode)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("making %q: %w", rel, err)
	}

	modes[rel] = info.Mode().Perm()

	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("reading %q: %w", src, err)
	}

	for _, entry := range entries {
		err = copyInto(root, filepath.Join(src, entry.Name()), filepath.Join(rel, entry.Name()), modes)
		if err != nil {
			return err
		}
	}

	return nil
}

func copyFile(root *os.Root, src, rel string, info os.FileInfo) error {
	from, err := os.Open(src) //nolint:gosec // src is this shim's own cache entry, not a peer's path
	if err != nil {
		return fmt.Errorf("reading a cached artifact: %w", err)
	}

	defer func() { _ = from.Close() }()

	// Removed, then O_EXCL: an entry already at this name may be a symlink an
	// earlier artifact planted, and O_TRUNC would write through it.
	_ = root.Remove(rel)

	to, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("writing %q: %w", rel, err)
	}

	_, err = io.Copy(to, from)
	if err != nil {
		_ = to.Close()

		return fmt.Errorf("writing %q: %w", rel, err)
	}

	// Chmod on the descriptor, after the write, for wire.UnpackTree's reason:
	// OpenFile's mode argument is masked by this process's umask, which strips
	// exactly the executable bit the orchestrator hashes — and PackPaths ships
	// what is here back as the step's outputs, so the loss travels home.
	err = to.Chmod(info.Mode().Perm())
	if err != nil {
		_ = to.Close()

		return fmt.Errorf("writing %q: %w", rel, err)
	}

	// Closed explicitly: a delayed-allocation filesystem reports ENOSPC here
	// and nowhere else, and a truncated artifact would be cached as complete.
	err = to.Close()
	if err != nil {
		return fmt.Errorf("writing %q: %w", rel, err)
	}

	return nil
}

// tombstonePrefix names an entry the sweep has taken out of service.
//
// Eviction renames before it deletes, and that ordering is the whole of what
// makes a cache HIT trustworthy. os.RemoveAll is not atomic: interrupted —
// SIGKILLed by an OOM or a spot reclamation, or simply holding a child it
// cannot unlink — it leaves part of the tree behind UNDER THE DIGEST. Both
// readers ask only whether that name exists, and the offer they answer tells
// the orchestrator to send nothing, so a half-entry is served as whole to
// every later step asking for that digest, and the re-fetch that would repair
// it is exactly what the false hit prevents. A content-addressed cache never
// re-reads what it holds, so it never heals.
//
// A rename either happened or it did not. After one, the only name a reader
// can see is a complete entry, and the mess is under a name only the sweep
// looks at.
const tombstonePrefix = ".evicting-"

// evictArtifact takes one entry out of service and then deletes it.
//
// Renamed FIRST, and the entry is left alone entirely if that rename fails:
// deleting in place is the thing being avoided, so falling back to it would
// give up the property on exactly the disks least able to afford it. An entry
// that could not be renamed stays whole, stays counted, and is tried again by
// the next sweep.
func evictArtifact(entry string) error {
	tombstone, err := os.MkdirTemp(filepath.Dir(entry), tombstonePrefix+"*")
	if err != nil {
		return fmt.Errorf("evicting a cached artifact: %w", err)
	}

	// MkdirTemp made the name; rename needs it not to exist.
	err = os.Remove(tombstone)
	if err != nil {
		return fmt.Errorf("evicting a cached artifact: %w", err)
	}

	err = os.Rename(entry, tombstone)
	if err != nil {
		return fmt.Errorf("evicting a cached artifact: %w", err)
	}

	err = os.RemoveAll(tombstone)
	if err != nil {
		// The entry is already unreachable to every reader, which is the part
		// that mattered. What is left is garbage the next sweep counts and
		// tries again.
		return fmt.Errorf("removing an evicted artifact: %w", err)
	}

	return nil
}

// stagingPrefix names a transfer in flight. Shared with the sweep, which must
// leave another session's staging directory alone.
const stagingPrefix = ".partial-"

// stageArtifact makes the directory an incoming artifact is unpacked into
// before it is named by its digest.
func stageArtifact(cache string) (string, error) {
	err := os.MkdirAll(cache, 0o700)
	if err != nil {
		return "", fmt.Errorf("making the artifact cache: %w", err)
	}

	staging, err := os.MkdirTemp(cache, stagingPrefix+"*")
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

// placeIfHeld places a cached artifact, answering false when this worker does
// not hold it after all.
//
// An entry that vanishes mid-copy is a cache MISS, not a failure. The cache is
// shared by every shim on the worker — two placed steps are two sessions in
// one process, and the aws:// bootstrap runs one PROCESS per placed step over
// the same --root — so a sweep can evict what another session is reading, and
// no lock this end could take covers the cross-process case. The reader that
// loses that race fetches, which is what a miss already does.
func placeIfHeld(held, name, workdir string) (bool, error) {
	_, err := os.Stat(held)
	if err != nil {
		return false, nil //nolint:nilerr // not holding it is the ordinary answer, not a failure
	}

	err = placeHeldArtifact(held, name, workdir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	return true, nil
}

// placeHeldArtifact copies one artifact tree into a work directory, and marks
// it used so the sweep evicts what is coldest rather than what is merely
// oldest.
//
// The tree is the cache entry on a hit and the staging directory on a miss:
// after a transfer the caller places what it just unpacked, because that is
// the copy nobody else can take away.
func placeHeldArtifact(tree, name, workdir string) error {
	now := time.Now()
	_ = os.Chtimes(tree, now, now)

	root, err := os.OpenRoot(workdir)
	if err != nil {
		return fmt.Errorf("placing an artifact in %q: %w", workdir, err)
	}

	defer func() { _ = root.Close() }()

	return copyTree(root, filepath.Join(tree, name), name)
}

// errDigestMismatch is an artifact whose bytes are not the ones the digest
// names.
var errDigestMismatch = errors.New("an artifact does not match the digest it was sent under")

// unpackVerified unpacks a tar stream and refuses it unless the stream hashes
// to the digest it was sent under.
//
// The digest used to be a KEY and not a proof, on both planes, and this is the
// one place either could be checked. What that bought an attacker: the shim's
// listener is unauthenticated by design and, under the aws:// bootstrap, runs
// as ROOT, so an unprivileged local process that cannot exec it can still dial
// it, commit arbitrary content under a legitimate digest, and have a later
// step take that content as an input it never has to transfer — executing it
// as root while `steps runs where` reports 0 B, as though nothing was
// needed. The attacker-free half is duller and likelier: a transfer that
// completed but is wrong poisons this worker permanently, because eviction is
// by size and nothing ever re-reads what the cache holds.
//
// Hashed over the UNCOMPRESSED tar stream, which is where the orchestrator
// takes it (packArtifactToFile tees the hasher off PackPaths, inside the
// compressor), so the two are the same bytes on both planes and under either
// compression. It is deliberately not a digest of the unpacked TREE: the key
// this proves has to be the key the cache is addressed by, and a second
// content hash would be a different question with a different answer.
//
// The stream is consumed as it is unpacked rather than in a second pass, so
// verification costs one sha256 over bytes already in flight and never buffers
// an artifact that may be gigabytes.
func unpackVerified(reader io.Reader, dir, digest string, zstd bool) error {
	hasher := sha256.New()

	err := compress.Unpack(reader, zstd, func(r io.Reader) error {
		return wire.UnpackTree(io.TeeReader(r, hasher), dir)
	})
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	// After the unpack, never instead of it: UnpackTree is what reads the
	// stream, so nothing has been hashed until it has finished reading. The
	// tree is refused before it is placed, which is what matters — placing is
	// what puts the bytes where the step's command will run them.
	got := hex.EncodeToString(hasher.Sum(nil))
	if got != digest {
		return fmt.Errorf("%w: sent as %s, hashes as %s", errDigestMismatch, digest, got)
	}

	return nil
}
