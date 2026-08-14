package agent

// Keeping a model's file paths inside the step's working directory.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveAgentPath resolves rel (as given by the model) against dir and
// rejects any result that escapes dir — lexically (a crafted
// "../../etc/passwd" style path) and, once a target actually exists, by
// symlink (see rejectSymlinkEscape).
//
// rel may be absolute: an oversized run_shell/custom-tool output spills to a
// file under the working directory and hands the model that file's absolute
// path (shell.SpillPointerMessage), and the working directory itself is an
// absolute path the model is told in its own system message — both are
// spellings of a location already inside dir, not an escape attempt. IsAbs
// used to reject both outright, which is why maxReadFileBytes' stated purpose
// ("a spilled tool output can be read back whole in one call") never actually
// worked: the containment check below is what makes an absolute path safe, so
// restricting rel to a relative spelling was never load-bearing — it only
// blocked the legitimate case along with the escaping one.
func resolveAgentPath(dir, rel string) (string, error) {
	resolved := filepath.Clean(rel)
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Clean(filepath.Join(dir, rel))
	}

	base := filepath.Clean(dir)

	if !within(base, resolved) {
		return "", fmt.Errorf("path %q escapes the working directory", rel)
	}

	err := rejectSymlinkEscape(base, resolved, rel)
	if err != nil {
		return "", err
	}

	return resolved, nil
}

// within reports whether path is base itself or sits under it.
func within(base, path string) bool {
	return path == base || strings.HasPrefix(path, base+string(os.PathSeparator))
}

// rejectSymlinkEscape re-validates resolved (already confined lexically by
// resolveAgentPath) against every symlink actually present on disk: the
// lexical check is a pure string comparison, so it's satisfied by "dir/leak"
// even when leak is a symlink pointing anywhere on the host — planted, for
// instance, via run_shell (which has no path confinement of its own) running
// `ln -s /etc/passwd leak` before a read_file("leak") call. EvalSymlinks
// resolves every symlink in resolved (mirroring shell/docker.go's
// resolveMountPath) and the result is re-checked against dir's own resolved
// form.
//
// A resolved path that does not yet exist is not treated as an escape:
// read_file/list_dir will fail with their own "not found" error when they try
// to open it, and there is nothing to leak from a path with no target.
func rejectSymlinkEscape(base, resolved, rel string) error {
	realResolved, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("%w", err)
	}

	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	if !within(realBase, realResolved) {
		return fmt.Errorf("path %q escapes the working directory (resolves to %q via a symlink)", rel, realResolved)
	}

	return nil
}

// resolveWritePath resolves rel like resolveAgentPath, then closes a gap
// resolveAgentPath alone leaves open for a brand-new file: EvalSymlinks fails
// with ENOENT on a nonexistent leaf regardless of whether an ancestor
// directory is a symlink (rejectSymlinkEscape then treats that as "nothing to
// leak"), so a target file that doesn't exist yet would otherwise skip the
// escape check entirely even when its parent directory is a symlink planted
// to point outside dir.
//
// When the target does not exist yet it creates missing parent directories
// (mkdir -p), but only after verifying the closest EXISTING ancestor is
// within the workspace.
func resolveWritePath(dir, rel string) (string, error) {
	resolved, err := resolveAgentPath(dir, rel)
	if err != nil {
		return "", err
	}

	_, err = os.Lstat(resolved)
	if err == nil {
		return resolved, nil // target exists; resolveAgentPath already covered it
	}

	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w", err)
	}

	ancestor := closestExisting(resolved)
	if ancestor == resolved {
		return "", fmt.Errorf("write_file: could not find an existing ancestor of %q", rel)
	}

	err = rejectSymlinkEscape(filepath.Clean(dir), ancestor, rel)
	if err != nil {
		return "", err
	}

	parent := filepath.Dir(resolved)

	err = os.MkdirAll(parent, 0o750)
	if err != nil {
		return "", fmt.Errorf("write_file: creating parent directory %q: %w", filepath.Dir(rel), err)
	}

	return resolved, nil
}

// closestExisting walks up from path's parent to the first directory that
// exists, so the symlink check runs against something real. path itself comes
// back only if nothing above it exists, which cannot happen for a path under
// an existing workspace — the caller guards anyway.
func closestExisting(path string) string {
	for ancestor := filepath.Dir(path); ancestor != path; ancestor = filepath.Dir(ancestor) {
		_, err := os.Stat(ancestor)
		if err == nil {
			return ancestor
		}
	}

	return path
}
