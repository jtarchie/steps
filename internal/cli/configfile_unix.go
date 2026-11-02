//go:build unix

package cli

import (
	"fmt"
	"os"
	"syscall"
)

// checkOwnedByUs: a file writable only by its owner is still that owner's to change, so another user's file is refused.
func checkOwnedByUs(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if ok && int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("web: --config %s is owned by uid %d, not the user running steps (uid %d); this file configures what the daemon runs", path, stat.Uid, os.Getuid())
	}

	return nil
}
