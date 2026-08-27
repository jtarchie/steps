package wire

// A step's directory tree, as bytes on a wire.
//
// This file exists to satisfy exactly one contract, and it is not tar's:
// internal/workspace's digestTree decides whether a step's outputs are the
// same content it cached last time, and it hashes a deliberately small set of
// facts — the set of relative paths, each path's kind, each regular file's
// executable bit and bytes, and each symlink's target text. A tree that comes
// back over this codec digesting differently than it went out does not fail
// loudly; it silently misses the step cache forever, or worse, serves a later
// step content that is no longer there.
//
// So every decision below is made against that list rather than against
// fidelity in general. Fields digestTree ignores (mtimes, ownership, the
// non-executable permission bits) are normalized away, because carrying them
// would make the same tree produce different bytes on different machines
// without changing anything a cache key can see. Fields digestTree hashes are
// carried exactly, including the ones tar makes it easy to lose.

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// stagedDirMode is what a directory is created with while a tree is being
// unpacked, before its recorded mode is applied.
//
// Private, and narrower than anything an archive is likely to carry: a
// directory is briefly visible between its creation and the second pass, and
// the safe end to err on is the closed one. A recorded mode wider than this is
// applied once the tree is whole.
const stagedDirMode = 0o700

// PackTree writes root's contents to w as a tar stream carrying exactly what
// digestTree hashes.
//
// Symlinks are stored as symlinks, never followed: following one would put the
// host's own files on the far end of the wire, which is the same reason
// digestTree hashes a link's target text rather than its destination.
//
// An entry that is neither a directory, a regular file nor a symlink — a
// socket, a fifo, a device node — is refused BY NAME rather than skipped.
// Skipping would drop a path digestTree counts, which changes the tree's
// digest, which poisons the step cache silently. Refusing is the honest
// failure, and in practice names a leftover .sock the step did not mean to
// declare as output.
func PackTree(w io.Writer, root string) error {
	return PackPaths(w, root, nil)
}

// PackPaths is PackTree restricted to the named top-level entries, each packed
// under its own name so it extracts back to the same place.
//
// A nil or empty names packs everything. Naming them is what keeps a step's
// declared outputs from dragging its inputs home: a two-gigabyte checkout does
// not need to travel back to prove it did not change.
//
// A named entry that does not exist is not an error here. A step that declared
// an output and produced nothing is a pipeline-level fact, reported where the
// outputs are checked, and failing the transfer instead would replace that
// message with a worse one.
func PackPaths(w io.Writer, root string, names []string) error {
	writer := tar.NewWriter(w)

	roots := []string{root}
	if len(names) > 0 {
		roots = make([]string, 0, len(names))

		for _, name := range names {
			err := packName(name)
			if err != nil {
				return fmt.Errorf("packing %q: %w", root, err)
			}

			roots = append(roots, filepath.Join(root, name))
		}
	}

	for _, from := range roots {
		err := packWalk(writer, root, from)
		if err != nil {
			return fmt.Errorf("packing %q: %w", root, err)
		}
	}

	// Close, not Flush: tar.Writer verifies here that every header's declared
	// size matched the bytes actually written, which is the same invariant
	// digestFile enforces when it refuses a file that changed under it.
	err := writer.Close()
	if err != nil {
		return fmt.Errorf("packing %q: %w", root, err)
	}

	return nil
}

// packName refuses a name that would pack a tree from outside root.
//
// unpackName's twin, and needed for the same reason on the other side: these
// names arrive from the PEER — the shim tars whatever a fetch frame asked for
// — so without this a FrameFetch for "../../.ssh" walks a tree outside the
// work directory and ships it back as data frames. unpackName cannot help
// there: it runs on the orchestrator, and whoever sent the frame is reading
// the raw stream.
func packName(name string) error {
	clean := filepath.Clean(name)
	if clean == "" || clean == "." || clean == ".." ||
		filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %q", ErrUnsafePath, name)
	}

	return nil
}

// packWalk writes the tree at from into writer, naming every entry relative to
// root so a subtree extracts back where it came from.
func packWalk(writer *tar.Writer, root, from string) error {
	_, err := os.Lstat(from)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	err = filepath.WalkDir(from, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("%w", err)
		}

		if rel == "." {
			return nil
		}

		return packEntry(writer, path, filepath.ToSlash(rel), entry)
	})
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

func packEntry(writer *tar.Writer, path, name string, entry fs.DirEntry) error {
	switch {
	case entry.IsDir():
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("%w", err)
		}

		return writeHeader(writer, &tar.Header{
			Typeflag: tar.TypeDir,
			Name:     name + "/",
			Mode:     int64(info.Mode().Perm()),
		})
	case entry.Type()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("%w", err)
		}

		return writeHeader(writer, &tar.Header{
			Typeflag: tar.TypeSymlink,
			Name:     name,
			Linkname: target,
		})
	case entry.Type().IsRegular():
		return packFile(writer, path, name, entry)
	default:
		return fmt.Errorf("%w: %q", ErrUnsupportedEntry, name)
	}
}

// ErrUnsupportedEntry is a socket, fifo or device node in a tree being packed.
// See PackTree: dropping it would change what digestTree computes over the
// extracted copy, and a cache that quietly disagrees with itself is worse than
// a step that refuses to ship one.
var ErrUnsupportedEntry = errors.New("cannot transfer a socket, fifo or device node")

func packFile(writer *tar.Writer, path, name string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	// The mode as it stands, not a canonical one. digestTree reads only the
	// executable bit, so this is invisible to every cache key — which is
	// precisely why collapsing it went unnoticed: a 0600 deploy key arrived on
	// the worker world-readable, and ssh refuses to use such a key at all.
	//
	// Reproducibility is unaffected. The bits come from the file, not from the
	// umask of whichever machine created it, so packing one tree twice still
	// produces one stream.
	mode := int64(info.Mode().Perm())

	size := info.Size()

	err = writeHeader(writer, &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     mode,
		Size:     size,
	})
	if err != nil {
		return err
	}

	file, err := os.Open(path) //nolint:gosec // path comes from WalkDir over the tree being packed
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { _ = file.Close() }()

	copied, err := io.Copy(writer, file)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	// Same reasoning as digestFile's identical check: the header already
	// committed to a length, so a file that grew or shrank between the stat
	// and the read would produce a stream that cannot be read back rather than
	// merely stale bytes.
	if copied != size {
		return fmt.Errorf("file %q changed while being packed (%d bytes, expected %d)", name, copied, size)
	}

	return nil
}

func writeHeader(writer *tar.Writer, header *tar.Header) error {
	// PAX explicitly: USTAR truncates names at 100 bytes and GNU at 256, which
	// a nested dependency tree reaches in ordinary use. Setting it here rather
	// than letting the writer choose means the format cannot change under us
	// because a name got longer.
	header.Format = tar.FormatPAX

	err := writer.WriteHeader(header)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// UnpackTree extracts a PackTree stream into root, which must already exist.
//
// Every path is opened through os.Root, so no entry can write outside root —
// including through a symlink an earlier entry in the same archive created,
// which is the shape workspace.rejectSymlinkSrc refuses on the way out and
// which has to be refused on the way in too, or the protection sits on the
// wrong side of the trust boundary.
func UnpackTree(r io.Reader, root string) error {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("unpacking into %q: %w", root, err)
	}
	defer func() { _ = dir.Close() }()

	reader := tar.NewReader(r)

	// Directory modes are applied only once every entry has been written. A
	// recorded 0500 restored on the way past would refuse its own children,
	// and a directory is briefly world-visible if created wide and narrowed
	// later — so they are created private (stagedDirMode) and opened up, never
	// the other way round.
	dirModes := map[string]fs.FileMode{}

	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return applyDirModes(dir, dirModes)
		}

		if err != nil {
			return fmt.Errorf("unpacking into %q: %w", root, err)
		}

		name, err := unpackName(header.Name)
		if err != nil {
			return fmt.Errorf("unpacking into %q: %w", root, err)
		}

		err = unpackEntry(dir, reader, header, name)
		if err != nil {
			return fmt.Errorf("unpacking %q into %q: %w", name, root, err)
		}

		if header.Typeflag == tar.TypeDir {
			dirModes[name] = fs.FileMode(header.Mode).Perm() //nolint:gosec // a mode this codec wrote
		}
	}
}

// applyDirModes restores recorded directory modes, deepest first so a
// directory narrowed by its own mode is not one this still has to write into.
func applyDirModes(dir *os.Root, modes map[string]fs.FileMode) error {
	names := make([]string, 0, len(modes))
	for name := range modes {
		names = append(names, name)
	}

	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	for _, name := range names {
		err := dir.Chmod(name, modes[name])
		if err != nil {
			return fmt.Errorf("restoring the mode of %q: %w", name, err)
		}
	}

	return nil
}

// ErrUnsafePath is an archive entry naming a path outside the tree. os.Root
// would refuse it anyway; this refuses it earlier and says which entry, since
// "path escapes from parent" alone does not name the archive that carried it.
var ErrUnsafePath = errors.New("archive entry names a path outside the tree")

func unpackName(name string) (string, error) {
	clean := strings.TrimSuffix(name, "/")
	if clean == "" || filepath.IsAbs(clean) || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("%w: %q", ErrUnsafePath, name)
	}

	return filepath.FromSlash(clean), nil
}

func unpackEntry(dir *os.Root, reader *tar.Reader, header *tar.Header, name string) error {
	switch header.Typeflag {
	case tar.TypeDir:
		return mkdirAll(dir, name)
	case tar.TypeSymlink:
		err := mkdirAll(dir, filepath.Dir(name))
		if err != nil {
			return err
		}

		// os.Root.Symlink does not validate the target, so an absolute or
		// escaping target round-trips verbatim — which is what digestTree
		// hashes. Writing THROUGH the link later is what os.Root refuses, and
		// that is the operation that was ever dangerous.
		return dir.Symlink(header.Linkname, name) //nolint:wrapcheck // the caller names the entry and the tree
	case tar.TypeReg:
		return unpackFile(dir, reader, header, name)
	default:
		return fmt.Errorf("%w: %q", ErrUnsupportedEntry, name)
	}
}

func unpackFile(dir *os.Root, reader *tar.Reader, header *tar.Header, name string) error {
	err := mkdirAll(dir, filepath.Dir(name))
	if err != nil {
		return err
	}

	// O_EXCL so an archive cannot overwrite something it already created, and
	// so a symlink planted by an earlier entry is never followed.
	file, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fs.FileMode(header.Mode)) //nolint:gosec // Mode is normalized to 0644/0755 by packFile
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	err = writeUnpacked(file, reader, header)

	// Checked, not dropped: this is a WRITE, and a delayed-allocation or
	// network filesystem reports ENOSPC only here. Swallowing it leaves a
	// truncated file that digestTree then hashes as the step's genuine
	// content — the silent divergence this file exists to prevent.
	closeErr := file.Close()
	if err == nil && closeErr != nil {
		return fmt.Errorf("finishing %q: %w", name, closeErr)
	}

	return err
}

func writeUnpacked(file *os.File, reader *tar.Reader, header *tar.Header) error {
	_, err := io.Copy(file, reader)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	// Chmod on the open descriptor, after the write: OpenFile's mode argument
	// is masked by the extracting process's umask, which would strip exactly
	// the executable bit digestTree hashes. Going through the descriptor also
	// avoids re-resolving the path, so nothing can be swapped underneath.
	err = file.Chmod(fs.FileMode(header.Mode)) //nolint:gosec // as above
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// mkdirAll creates name and its parents inside dir. os.Root has no MkdirAll,
// and walking the components is what keeps every level inside the root.
func mkdirAll(dir *os.Root, name string) error {
	if name == "." || name == "" {
		return nil
	}

	var built string

	for _, part := range strings.Split(name, string(filepath.Separator)) {
		built = filepath.Join(built, part)

		err := dir.Mkdir(built, stagedDirMode)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w", err)
		}
	}

	return nil
}
