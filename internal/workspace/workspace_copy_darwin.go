//go:build darwin

package workspace

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// copyTreeCandidates returns cp invocations to copy src's contents into an
// existing dst, in preference order. -c requests APFS clonefile
// (copy-on-write) and errors on filesystems that don't support it (e.g.
// exfat, or a non-APFS network mount), so the plain form is the fallback.
// -P never follows symlinks (copies the link itself); -p preserves
// mode/timestamps.
func copyTreeCandidates(src, dst string) [][]string {
	return [][]string{
		{"cp", "-c", "-R", "-P", "-p", src + "/.", dst},
		{"cp", "-R", "-P", "-p", src + "/.", dst},
	}
}

// cloneTree clones src's whole tree into dst, an empty directory it replaces, in one clonefile(2): measured 10.8x faster than cp -c's clone per file (1.9s against 20.6s for 100,000 files, 1.1 GiB). Apple's man page discourages it on directories, so any error leaves dst to copyTree's per-file fallback.
func cloneTree(src, dst string) error {
	err := os.Remove(dst)
	if err != nil {
		return fmt.Errorf("clearing %q for a clone: %w", dst, err)
	}

	err = unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
	if err == nil {
		return nil
	}

	// A clone that failed part way may have left a tree behind; cp -R src/. creates dst afresh.
	_ = os.RemoveAll(dst)

	return fmt.Errorf("clonefile %q: %w", src, err)
}
