//go:build linux

package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jtarchie/steps/internal/config"
)

// btrfsSuperMagic is BTRFS_SUPER_MAGIC (see linux/magic.h) — statfs's f_type
// on a btrfs filesystem.
const btrfsSuperMagic = 0x9123683e

// btrfsCmdTimeout bounds cleanup btrfs invocations (subvolume delete), which
// run from Close/remove paths that must not depend on a caller's (possibly
// already-canceled) context — see treeBackend.remove's doc comment.
const btrfsCmdTimeout = 30 * time.Second

// newBtrfsProvider builds the isolatingProvider for strategy: btrfs. Root is
// required (enforced by Config.validateWorkspace at load time) and is never
// owned/removed by the provider — only the build/step subvolumes created
// under it are.
func newBtrfsProvider(ws *config.WorkspaceConfig) Provider {
	return &isolatingProvider{
		backend: btrfsBackend{compression: ws.Options.Compression},
		validate: func() error {
			return validateBtrfsRoot(ws.Root)
		},
		root:     ws.Root,
		ownsRoot: false,
	}
}

// validateBtrfsRoot fails fast, at startup, on everything that would
// otherwise only surface as a confusing error mid-build: the btrfs CLI
// missing, root not on a btrfs filesystem (the system temp directory is
// commonly tmpfs, not btrfs), or root not creatable/writable.
func validateBtrfsRoot(root string) error {
	if root == "" {
		return errors.New("workspace.root is required for strategy: btrfs")
	}

	_, err := exec.LookPath("btrfs")
	if err != nil {
		return fmt.Errorf("strategy: btrfs requires the btrfs CLI on PATH: %w", err)
	}

	err = os.MkdirAll(root, 0o750)
	if err != nil {
		return fmt.Errorf("workspace.root %q: %w", root, err)
	}

	ok, err := isBtrfs(root)
	if err != nil {
		return fmt.Errorf("workspace.root %q: %w", root, err)
	}

	if !ok {
		return fmt.Errorf("workspace.root %q is not on a btrfs filesystem (the system temp directory is commonly tmpfs, not btrfs — set workspace.root to a btrfs mount)", root)
	}

	return nil
}

func isBtrfs(path string) (bool, error) {
	var st unix.Statfs_t

	err := unix.Statfs(path, &st)
	if err != nil {
		return false, fmt.Errorf("statfs %q: %w", path, err)
	}

	return int64(st.Type) == btrfsSuperMagic, nil
}

// btrfsBackend implements treeBackend over the btrfs CLI: subvolume
// create/snapshot are instant, copy-on-write operations, so an isolated
// step workspace materializes without copying any file content. Names
// reaching this backend are already regex-validated (see
// artifactNamePattern), so argv-form exec is safe from injection; commands
// are never run through a shell.
//
// Privileges: subvolume create/snapshot are unprivileged on modern kernels.
// subvolume delete by a non-root user requires the filesystem mounted with
// user_subvol_rm_allowed (kernel >= 4.18); remove falls back to a plain
// directory removal when delete fails, which still reclaims the space (an
// empty subvolume can be rmdir'd) even without that mount option — just
// without the instant subvolume-metadata cleanup.
type btrfsBackend struct {
	compression string
}

func (b btrfsBackend) createEmpty(ctx context.Context, dir string) error {
	err := runBtrfs(ctx, "subvolume", "create", dir)
	if err != nil {
		return err
	}

	return b.applyCompression(ctx, dir)
}

func (b btrfsBackend) materialize(ctx context.Context, src, dst string) error {
	err := runBtrfs(ctx, "subvolume", "snapshot", src, dst)
	if err != nil {
		return err
	}

	return b.applyCompression(ctx, dst)
}

// applyCompression sets a subvolume's compression property immediately
// after creating/snapshotting it (before anything is written into it), so
// the setting applies to everything the step subsequently writes.
func (b btrfsBackend) applyCompression(ctx context.Context, dir string) error {
	if b.compression == "" || b.compression == "none" {
		return nil
	}

	return runBtrfs(ctx, "property", "set", dir, "compression", b.compression)
}

func (btrfsBackend) remove(dir string) error {
	_, statErr := os.Stat(dir)
	if os.IsNotExist(statErr) {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), btrfsCmdTimeout)
	defer cancel()

	err := runBtrfs(ctx, "subvolume", "delete", dir)
	if err == nil {
		return nil
	}

	slog.Warn("workspace.btrfs_delete_fallback", "dir", dir, "error", err)

	removeErr := os.RemoveAll(dir)
	if removeErr != nil {
		return fmt.Errorf("btrfs subvolume delete %q failed (%w), and plain removal also failed: %w", dir, err, removeErr)
	}

	return nil
}

// btrfsSubvolInode is the inode number every btrfs subvolume's root
// directory carries (BTRFS_FIRST_FREE_OBJECTID), the reliable way to tell a
// subvolume apart from an ordinary directory by stat alone.
const btrfsSubvolInode = 256

// removeTree tears down dir, which is a plain directory that may *contain*
// subvolumes (a build root's artifact/step trees, a step dir's input
// snapshots and output subvolumes). It deletes those subvolumes explicitly
// with `btrfs subvolume delete` — deepest first — before os.RemoveAll takes
// the plain-directory skeleton, so cleanup never depends on rmdir(2) of a
// subvolume, which restrictive mounts (no user_subvol_rm_allowed) and
// pre-4.18 kernels deny. Subvolume deletion is best-effort: any failure is
// logged and the final os.RemoveAll still runs, so this is never worse than
// the plain removal it replaces.
func (btrfsBackend) removeTree(dir string) error {
	_, statErr := os.Stat(dir)
	if os.IsNotExist(statErr) {
		return nil
	}

	subvols := findSubvolumes(dir)

	if len(subvols) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), btrfsCmdTimeout)
		defer cancel()

		for _, sv := range subvols {
			err := runBtrfs(ctx, "subvolume", "delete", sv)
			if err != nil {
				slog.Warn("workspace.btrfs_delete_fallback", "dir", sv, "error", err)
			}
		}
	}

	err := os.RemoveAll(dir)
	if err != nil {
		return fmt.Errorf("could not remove %q: %w", dir, err)
	}

	return nil
}

// findSubvolumes returns every btrfs subvolume root at or below root, sorted
// deepest-path-first so a child subvolume is always deleted before the
// subvolume (or directory) that contains it.
func findSubvolumes(root string) []string {
	var subvols []string

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() {
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}

		st, ok := info.Sys().(*syscall.Stat_t)
		if ok && st.Ino == btrfsSubvolInode {
			subvols = append(subvols, path)
		}

		return nil
	})
	if walkErr != nil {
		// A walk error just means the deepest-first list may be incomplete;
		// os.RemoveAll in the caller is still the backstop, so proceed with
		// whatever was found rather than aborting cleanup.
		slog.Warn("workspace.btrfs_walk", "root", root, "error", walkErr)
	}

	sort.Slice(subvols, func(i, j int) bool { return len(subvols[i]) > len(subvols[j]) })

	return subvols
}

func runBtrfs(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "btrfs", args...) //nolint:gosec // argv-form exec over regex-validated, workspace-internal paths; never shelled out
	cmd.Stdin = nil

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs %s: %w: %s", strings.Join(args, " "), err, out)
	}

	return nil
}
