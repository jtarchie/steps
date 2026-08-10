package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/jtarchie/steps/internal/config"
)

// newCopyProvider builds the isolatingProvider for strategy: copy: portable
// across macOS and Linux, using cp for the actual materialization (see
// copyTree) so copy-on-write is used automatically wherever the underlying
// filesystem supports it. root is resolved once at construction time (see
// newIsolatingRoot) rather than lazily, so isolatingProvider always has a
// real, absolute path to build under.
func newCopyProvider(ws *config.WorkspaceConfig, keep bool) (Provider, error) {
	root, ownsRoot, err := newIsolatingRoot(ws.Root)
	if err != nil {
		return nil, err
	}

	provider := &isolatingProvider{
		backend: copyBackend{},
		validate: func() error {
			return validateRootWritable(root)
		},
		root:     root,
		ownsRoot: ownsRoot,
		keep:     keep,
		token:    newInvocationToken(),
	}

	err = provider.enableCache(ws)
	if err != nil {
		return nil, err
	}

	return provider, nil
}

// validateRootWritable probes root by creating and removing a temp
// directory inside it — root itself is already guaranteed to exist by
// newIsolatingRoot, but existence doesn't imply writability (e.g. a
// read-only mount).
func validateRootWritable(root string) error {
	probe, err := os.MkdirTemp(root, ".steps-probe-*")
	if err != nil {
		return fmt.Errorf("workspace root %q is not writable: %w", root, err)
	}

	err = os.RemoveAll(probe)
	if err != nil {
		return fmt.Errorf("could not remove probe directory %q: %w", probe, err)
	}

	return nil
}

// copyBackend implements treeBackend by shelling out to cp: correct
// symlink (never-follow), permission, and mode handling is exactly what cp
// already does, and copy-on-write (APFS clonefile, Linux reflink) comes for
// free where the filesystem supports it — a pure-Go filepath.WalkDir copy
// would get none of that. Names reaching this backend are already
// regex-validated (see artifactNamePattern), so argv-form exec is safe from
// injection; commands are never run through a shell.
type copyBackend struct{}

func (copyBackend) createEmpty(_ context.Context, dir string) error {
	return os.MkdirAll(dir, 0o750) //nolint:wrapcheck // caller (isolatingBuild/Space) wraps with context
}

func (copyBackend) materialize(ctx context.Context, src, dst string) error {
	err := os.MkdirAll(dst, 0o750)
	if err != nil {
		return fmt.Errorf("could not create %q: %w", dst, err)
	}

	return copyTree(ctx, src, dst)
}

func (copyBackend) remove(dir string) error {
	err := os.RemoveAll(dir)
	if err != nil {
		return fmt.Errorf("could not remove %q: %w", dir, err)
	}

	return nil
}

// removeTree is identical to remove for the copy backend: plain directories
// and copied trees are all removed the same way. btrfs overrides this to
// delete nested subvolumes first (see workspace_btrfs_linux.go).
func (b copyBackend) removeTree(dir string) error {
	return b.remove(dir)
}

// copyTree copies src's contents into dst (which must already exist),
// trying each platform candidate command line in order (see
// copyTreeCandidates) until one succeeds — earlier candidates prefer
// copy-on-write and fall back to a plain recursive copy if the filesystem
// doesn't support it.
func copyTree(ctx context.Context, src, dst string) error {
	var lastErr error

	for _, argv := range copyTreeCandidates(src, dst) {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv-form exec over regex-validated, workspace-internal paths; never shelled out
		cmd.Stdin = nil

		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}

		lastErr = fmt.Errorf("%s: %w: %s", argv[0], err, out)

		removeErr := os.RemoveAll(dst)
		if removeErr != nil {
			return fmt.Errorf("copy %q to %q failed (%w), and cleanup also failed: %w", src, dst, lastErr, removeErr)
		}

		mkdirErr := os.MkdirAll(dst, 0o750)
		if mkdirErr != nil {
			return fmt.Errorf("copy %q to %q failed (%w), and recreating %q also failed: %w", src, dst, lastErr, dst, mkdirErr)
		}
	}

	return fmt.Errorf("copy %q to %q: %w", src, dst, lastErr)
}

// CopyTree duplicates a directory tree, exported for the one caller outside
// this package: a replay forks the workspace of the run it re-executes, rather
// than editing the baseline it is being compared against.
//
// Same implementation the copy strategy uses for step isolation, so a replay
// inherits its platform-specific fast paths (clonefile, reflink) instead of
// growing a second, slower copier.
func CopyTree(ctx context.Context, src, dst string) error {
	return copyTree(ctx, src, dst)
}
