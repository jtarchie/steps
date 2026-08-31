package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// errLossyRoot is a workspace root whose filesystem cannot store a file's
// executable bit.
//
//nolint:gochecknoglobals // an error value, and one a caller matches on
var errLossyRoot = errors.New("the workspace root's filesystem cannot hold an executable bit")

// rootExecBit is whether a directory's filesystem can hold a file's
// executable bit.
//
// A package variable so a test can drive the refusal all the way through
// Provider.Validate: no filesystem a test can mount fails the probe, and the
// failure being guarded against is a probe nobody consults.
//
//nolint:gochecknoglobals // a test seam for a filesystem probe
var rootExecBit = rootExecBitAt

// rootExecBitAt creates a file in dir, sets 0700 on it, reads the mode back,
// and removes it — answering nil when it could not do that at all.
//
// A second copy of the probe internal/shim runs on a worker, rather than a
// shared one, because the two answer for different machines and the
// dependency graph keeps them apart: shim speaks the venue's wire protocol
// and workspace does not import it outside tests. The duplication is the
// twenty lines below; sharing it would be a new package for one function on
// either side of a boundary that exists on purpose.
func rootExecBitAt(dir string) *bool {
	return rootExecBitVia(dir, func(name string) (fs.FileMode, error) {
		info, err := os.Stat(name)
		if err != nil {
			return 0, err //nolint:wrapcheck // the caller only asks whether there was an answer
		}

		return info.Mode().Perm(), nil
	})
}

// rootExecBitVia is rootExecBitAt with the read-back injected, because the
// answer turns entirely on reading the mode back and no filesystem a test can
// mount disagrees with the chmod that set it.
func rootExecBitVia(dir string, stat func(string) (fs.FileMode, error)) *bool {
	probe := filepath.Join(dir, ".steps-execbit-probe")

	// Removed first, not only last: a probe left behind by a crash would
	// otherwise be opened with its existing mode and answer about that
	// rather than about the filesystem.
	_ = os.Remove(probe)

	file, err := os.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // probe is a fixed name under a directory this process just made
	if err != nil {
		return nil
	}

	_ = file.Close()

	defer func() { _ = os.Remove(probe) }()

	err = os.Chmod(probe, 0o700) //nolint:gosec // 0700 is the question being asked; a mode gosec would prefer cannot answer it
	if err != nil {
		return nil
	}

	// Read back rather than trusting the Chmod's own error, because the
	// filesystems this exists for do not report one: a vfat or exfat mount
	// synthesizes a mode from fmask/dmask and accepts the chmod silently,
	// as do CIFS without unix extensions and 9p/virtiofs. The mode
	// afterwards is the only question they answer honestly.
	mode, err := stat(probe)
	if err != nil {
		return nil
	}

	held := mode&0o111 != 0

	return &held
}

// validateRootHoldsExecBit refuses a root whose filesystem would strip a mode
// that digestTree hashes as content.
//
// Refused rather than warned, matching what the venue does to a worker
// answering the same way: a tree materialized under such a root and digested
// again is content that changed for no reason a reader can point at, and the
// step cache cannot tell a stripped bit from an edit. Silence — a probe that
// could not run at all — is not a refusal, because a root that cannot be
// written to is a different failure, and validateRootWritable above says it
// far better than this would.
func validateRootHoldsExecBit(root string) error {
	held := rootExecBit(root)
	if held != nil && !*held {
		return fmt.Errorf("%w: %q did not give back the executable bit just set on a file there — every tree materialized under it comes back missing one, and the step cache reads that as an edit. Point workspace.root: at a filesystem that can hold one",
			errLossyRoot, root)
	}

	return nil
}
