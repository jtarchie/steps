package resource

// Unpacking the tree GitHub serves for one commit into an artifact.
//
// What the artifact digest sees is what has to come out right: every path,
// whether a file is executable, and what a symlink says — hashed as the
// link's own text and never followed (internal/workspace's digestTree). So
// the exec bit and the links are kept exactly, and everything else about an
// entry (owner, mtime) is left to this machine, as a checkout would.

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// githubTreeLimit bounds what one tarball may unpack to. A repository this
// large wants a git resource and a shallow clone, not an archive held whole.
const githubTreeLimit int64 = 8 << 30

var errTreeTooLarge = errors.New("the tree is larger than the unpack limit")

// unpackTarball writes a GitHub tarball's tree into destDir.
//
// Every write goes through an os.Root, which refuses any path that would
// resolve outside destDir — a "../" entry, an absolute one, or a later entry
// written THROUGH an earlier symlink that points away. A symlink itself may
// say anything: a repository is allowed one pointing at /etc, and writing it
// is not following it.
func unpackTarball(archive io.Reader, destDir string) error {
	return unpackTarballWithin(archive, destDir, githubTreeLimit)
}

func unpackTarballWithin(archive io.Reader, destDir string, limit int64) error {
	unzipped, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("tarball: %w", err)
	}

	defer func() { _ = unzipped.Close() }()

	root, err := os.OpenRoot(destDir)
	if err != nil {
		return fmt.Errorf("tarball: %w", err)
	}

	defer func() { _ = root.Close() }()

	reader := tar.NewReader(unzipped)
	budget := limit

	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("tarball: %w", err)
		}

		name, keep := treePath(header.Name)
		if !keep {
			continue
		}

		err = unpackEntry(root, reader, header, name, &budget)
		if err != nil {
			return fmt.Errorf("tarball: %s: %w", name, err)
		}
	}
}

// treePath strips the one top-level directory GitHub wraps every entry in,
// <owner>-<repo>-<short sha>/, and reports false for that directory itself.
func treePath(name string) (string, bool) {
	_, rest, found := strings.Cut(name, "/")
	if !found {
		return "", false
	}

	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		return "", false
	}

	return rest, true
}

func unpackEntry(root *os.Root, reader io.Reader, header *tar.Header, name string, budget *int64) error {
	switch header.Typeflag {
	case tar.TypeXGlobalHeader:
		// GitHub puts the commit in a pax global header; it describes the
		// archive and is not an entry of the tree.
		return nil
	case tar.TypeDir:
		return mkdirAll(root, name)
	case tar.TypeReg:
		return unpackFile(root, reader, header, name, budget)
	case tar.TypeSymlink:
		err := mkdirAll(root, path.Dir(name))
		if err != nil {
			return err
		}

		return wrapIfErr(root.Symlink(header.Linkname, name))
	default:
		return fmt.Errorf("entry type %q is not one a repository tree holds", string(header.Typeflag))
	}
}

func unpackFile(root *os.Root, reader io.Reader, header *tar.Header, name string, budget *int64) error {
	*budget -= header.Size
	if *budget < 0 {
		return errTreeTooLarge
	}

	err := mkdirAll(root, path.Dir(name))
	if err != nil {
		return err
	}

	// Readable by everyone, as a clone would leave it: a containerized step
	// may run as another user than the one that fetched the tree.
	mode := os.FileMode(0o644)
	if header.Mode&0o111 != 0 {
		mode = 0o755
	}

	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	_, err = io.CopyN(file, reader, header.Size)
	if err != nil {
		_ = file.Close()

		return fmt.Errorf("%w", err)
	}

	return wrapIfErr(file.Close())
}

func mkdirAll(root *os.Root, dir string) error {
	if dir == "." || dir == "" {
		return nil
	}

	return wrapIfErr(root.MkdirAll(dir, 0o755))
}

func wrapIfErr(err error) error {
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}
