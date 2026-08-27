//go:build linux

package shim

import (
	"fmt"
	"syscall"
)

// fsMagics names the filesystems worth naming, by the f_type statfs reports.
//
// Deliberately short. This table exists so the two questions the orchestrator
// asks — is this memory, is this btrfs — have names rather than numbers; every
// other filesystem is reported as its magic, which is what an operator would
// search for anyway.
//
// Keyed by uint32, which is what a magic IS: syscall.Statfs_t.Type is int64 on
// linux/amd64 and linux/arm64 but int32 on linux/386 and linux/arm, so an
// int64 conversion sign-extends every magic with the high bit set — btrfs and
// ramfs among them — and the lookup misses on exactly the 32-bit workers the
// table is meant to name.
//
//nolint:gochecknoglobals // a table of kernel constants, not state
var fsMagics = map[uint32]string{
	0x9123683e: "btrfs",
	0x01021994: "tmpfs",
	0x858458f6: "ramfs",
	0xef53:     "ext", // ext2, ext3 and ext4 share one magic
	0x58465342: "xfs",
	0x794c7630: "overlayfs",
	0x2fc12fc1: "zfs",
	0x6969:     "nfs",
}

// fsInfoAt names the filesystem at path and how many bytes are available on it.
//
// An error is reported as silence rather than as a guess: the caller's whole
// contract is that empty means "cannot say".
func fsInfoAt(path string) (string, uint64) {
	var stat syscall.Statfs_t

	err := syscall.Statfs(path, &stat)
	if err != nil {
		return "", 0
	}

	magic := uint32(stat.Type) //nolint:gosec // a 32-bit magic, however the platform widened it

	name, known := fsMagics[magic]
	if !known {
		name = fmt.Sprintf("%#x", magic)
	}

	return name, stat.Bavail * uint64(stat.Bsize)
}
