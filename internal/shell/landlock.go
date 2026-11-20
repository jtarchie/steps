package shell

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// landlockAccess is what one Landlock rule grants beneath its path.
type landlockAccess int

const (
	// listDir lists a directory's names and grants nothing beneath it: an ancestor of a hidden path.
	listDir landlockAccess = iota
	readTree
	readFile
	writeTree
	writeFile
)

type landlockGrant struct {
	path   string
	access landlockAccess
}

// landlockDevices are the device files an ordinary shell writes, the linux spelling of seatbeltDevices. A pipe or socket on an inherited fd is not a path, so Landlock never asks about it.
var landlockDevices = []landlockGrant{ //nolint:gochecknoglobals // static, read-only list
	{"/dev/null", writeFile}, {"/dev/zero", writeFile}, {"/dev/tty", writeFile}, {"/dev/pts", writeTree},
}

// landlockGrants turns the fence's policy into Landlock's allow-only rules. Landlock cannot deny beneath an allow, so "read everything but the credentials" is spelled as: each ancestor of a hidden path may be listed, and every OTHER child of it is readable as a whole tree. A read grant on an ancestor would cover the hidden path beneath it. Symlink children get no rule: the kernel checks the path a link resolves to, which this walk already covers or hides.
func landlockGrants(writable, hidden []string) ([]landlockGrant, error) {
	hide, ancestors := hiddenTree(hidden)

	for _, dir := range writable {
		for path := range hide {
			if dir == string(filepath.Separator) || path == dir || strings.HasPrefix(path, dir+string(filepath.Separator)) {
				return nil, fmt.Errorf("sandbox: %s is writable, and Landlock cannot hide %s beneath it", dir, path)
			}
		}
	}

	grants := make([]landlockGrant, 0, len(writable)+len(landlockDevices))

	for _, dir := range sortedKeys(ancestors) {
		if !hide[dir] {
			grants = append(grants, ancestorGrants(dir, hide, ancestors)...)
		}
	}

	for _, dir := range writable {
		grants = append(grants, landlockGrant{dir, writeTree})
	}

	return append(grants, landlockDevices...), nil
}

// hiddenTree is each hidden path by its real path, and every directory above one.
func hiddenTree(hidden []string) (map[string]bool, map[string]bool) {
	hide := map[string]bool{}
	ancestors := map[string]bool{}

	for _, path := range hidden {
		resolved := realPath(path)
		hide[resolved] = true

		for dir := filepath.Dir(resolved); !ancestors[dir]; dir = filepath.Dir(dir) {
			ancestors[dir] = true
		}
	}

	return hide, ancestors
}

// ancestorGrants lists dir and reads each child that is neither hidden nor above a hidden path.
func ancestorGrants(dir string, hide, ancestors map[string]bool) []landlockGrant {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // missing, or not ours to list: nothing beneath it is granted, which is the stricter answer
	}

	grants := []landlockGrant{{dir, listDir}}

	for _, entry := range entries {
		child := filepath.Join(dir, entry.Name())

		switch {
		case hide[child], ancestors[child], entry.Type()&os.ModeSymlink != 0:
		case entry.IsDir():
			grants = append(grants, landlockGrant{child, readTree})
		default:
			grants = append(grants, landlockGrant{child, readFile})
		}
	}

	return grants
}

// realPath is path with every symlink in its existing prefix resolved, so a hidden path that does not exist yet still names the directory it would appear in.
func realPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved
	}

	parent := filepath.Dir(path)
	if parent == path || !errors.Is(err, os.ErrNotExist) {
		return path
	}

	return filepath.Join(realPath(parent), filepath.Base(path))
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	return keys
}
