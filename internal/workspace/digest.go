package workspace

// Content digests: what an artifact directory actually HOLDS, as opposed to
// what the plan says produced it.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Entry kinds, written into the digest stream so a file, a directory and a
// symlink at the same path can never hash alike.
const (
	digestKindDir     = 'd'
	digestKindFile    = 'f'
	digestKindSymlink = 'l'
	digestKindOther   = 'o'
)

// digestTree is the content hash of a directory tree: every entry's path, kind
// and bytes, and nothing else.
//
// Deliberately excluded: mtimes, ownership, and inode numbers. Two checkouts
// of one commit, made at different times into different directories, are the
// same content — a digest that disagreed would miss on every run, which is the
// same as having no cache at all.
//
// Included, because each changes what the next command sees: the executable
// bit (a script that can no longer be run is not the same input), symlink
// targets (hashed as the link's own text, never followed — following would let
// a link into /etc pull host state into a cache key), and the set of paths
// itself (an empty file and an absent file must not collide).
//
// Every variable-length field is length-prefixed, so no arrangement of paths
// and file contents can produce another tree's byte stream.
func digestTree(root string) (string, error) {
	sum := sha256.New()

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
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

		// ToSlash so an artifact digested on one platform matches the same
		// bytes digested on another.
		digestField(sum, []byte(filepath.ToSlash(rel)))

		return digestEntry(sum, path, entry)
	})
	if err != nil {
		return "", fmt.Errorf("digesting %q: %w", root, err)
	}

	return hashHex(sum), nil
}

// digestEntry writes one directory entry's kind and content into sum.
func digestEntry(sum hash.Hash, path string, entry fs.DirEntry) error {
	switch {
	case entry.IsDir():
		digestByte(sum, digestKindDir)
	case entry.Type()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("%w", err)
		}

		digestByte(sum, digestKindSymlink)

		// ToSlash for the same reason the entry's own path gets it: a link
		// target IS a path, and the same logical link would otherwise hash one
		// way where separators are slashes and another where they are
		// backslashes. Free on this side — ToSlash is the identity wherever
		// Separator is '/', so no digest that already exists moves, and a
		// backslash in a POSIX target stays the ordinary filename character it
		// is.
		digestField(sum, []byte(filepath.ToSlash(target)))
	case entry.Type().IsRegular():
		digestByte(sum, digestKindFile)

		return digestFile(sum, path, entry)
	default:
		// A socket, fifo or device node carries no content a later step can
		// meaningfully read back from a cache entry. Its presence is recorded;
		// its identity is not.
		digestByte(sum, digestKindOther)
	}

	return nil
}

// digestFile writes a regular file's executable bit and bytes into sum.
func digestFile(sum hash.Hash, path string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	// Only the executable bit, not the whole mode: umask differences between
	// the run that filled the cache and the one reading it would otherwise
	// miss on every entry, while whether a file can be RUN genuinely changes
	// what the next step can do with it.
	var exec byte
	if info.Mode()&0o111 != 0 {
		exec = 1
	}

	digestByte(sum, exec)

	size := info.Size()
	digestLength(sum, uint64(size)) //nolint:gosec // a regular file's size is never negative

	file, err := os.Open(path) //nolint:gosec // path comes from WalkDir over the directory being digested
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { _ = file.Close() }()

	copied, err := io.Copy(sum, file)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	// The length prefix is what makes the stream unambiguous, so a file that
	// changed size between the stat and the read would silently invalidate
	// that guarantee rather than merely digest stale bytes.
	if copied != size {
		return fmt.Errorf("file %q changed while being digested (%d bytes, expected %d)", path, copied, size)
	}

	return nil
}

// digestField writes a length-prefixed variable-length field.
func digestField(sum hash.Hash, field []byte) {
	digestLength(sum, uint64(len(field)))
	_, _ = sum.Write(field)
}

func digestLength(sum hash.Hash, n uint64) {
	var prefix [8]byte

	binary.BigEndian.PutUint64(prefix[:], n)
	_, _ = sum.Write(prefix[:])
}

func digestByte(sum hash.Hash, b byte) {
	_, _ = sum.Write([]byte{b})
}

// hashHex renders a hash's current sum the way every other key in this
// codebase is spelled.
func hashHex(sum hash.Hash) string {
	return hex.EncodeToString(sum.Sum(nil))
}
