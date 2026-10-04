// Package treedigest lets a worker with no steps code (busybox only) name a tree exactly as the orchestrator does, so outputs can stay where they were made (steps#206).
package treedigest

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Script is Tree for busybox: run as `sh -c "$Script" sh ROOT`, it prints the same digest.
//
//go:embed digest.sh
var Script string

// Image is pinned by digest: a moved tag could change what sort or od print, and the manifest format is a contract.
const Image = "busybox:1.37@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

// Tree excludes mtimes, ownership and non-execute mode bits so two checkouts of one commit name alike.
func Tree(root string) (string, error) {
	return treeOf(rawFS(root))
}

func treeOf(fsys fs.FS) (string, error) {
	var lines []string

	err := fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if path == "." {
			return nil
		}

		line, err := manifestLine(fsys, path, entry)
		if err != nil {
			return fmt.Errorf("digesting %q: %w", path, err)
		}

		lines = append(lines, line)

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("digesting tree: %w", err)
	}

	slices.Sort(lines)

	sum := sha256.New()
	for _, line := range lines {
		_, _ = io.WriteString(sum, line+"\n")
	}

	return hex.EncodeToString(sum.Sum(nil)), nil
}

func manifestLine(fsys fs.FS, path string, entry fs.DirEntry) (string, error) {
	name := hex.EncodeToString([]byte(path))

	switch kind := entry.Type(); {
	case kind.IsDir():
		return name + " d", nil
	case kind&fs.ModeSymlink != 0:
		target, err := fs.ReadLink(fsys, path)
		if err != nil {
			return "", fmt.Errorf("%w", err)
		}

		return name + " l " + hex.EncodeToString([]byte(target)), nil
	case kind.IsRegular():
		info, err := entry.Info()
		if err != nil {
			return "", fmt.Errorf("%w", err)
		}

		exec := "-"
		if info.Mode()&0o111 != 0 {
			exec = "x"
		}

		sum, err := fileSum(fsys, path)
		if err != nil {
			return "", err
		}

		return strings.Join([]string{name, "f", exec, sum}, " "), nil
	default:
		return name + " o", nil
	}
}

func fileSum(fsys fs.FS, path string) (string, error) {
	file, err := fsys.Open(path)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	defer func() { _ = file.Close() }()

	sum := sha256.New()

	_, err = io.Copy(sum, file)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	return hex.EncodeToString(sum.Sum(nil)), nil
}

// rawFS skips the fs.ValidPath check os.DirFS makes, which refuses the non-UTF-8 names Linux allows.
type rawFS string

func (r rawFS) path(name string) string { return filepath.Join(string(r), filepath.FromSlash(name)) }

func (r rawFS) Open(name string) (fs.File, error) {
	file, err := os.Open(r.path(name))
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	return file, nil
}

func (r rawFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := os.ReadDir(r.path(name))
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	return entries, nil
}

func (r rawFS) ReadLink(name string) (string, error) {
	target, err := os.Readlink(r.path(name))
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	return target, nil
}

func (r rawFS) Lstat(name string) (fs.FileInfo, error) {
	info, err := os.Lstat(r.path(name))
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	return info, nil
}
