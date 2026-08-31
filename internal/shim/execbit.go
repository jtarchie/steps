package shim

import (
	"io/fs"
	"os"
	"path/filepath"
)

// execBit is whether a directory's filesystem can hold a file's executable
// bit, measured rather than inferred.
//
// A package variable for the reason fsInfo beside it is one: a test cannot
// mount a filesystem that strips modes, so substituting this is the only way
// to assert WHICH directory the hello probed, and the only way to drive the
// false answer through the code that reports it.
//
//nolint:gochecknoglobals // a test seam for a filesystem probe, as fsInfo is
var execBit = execBitAt

// execBitAt creates a file in dir, sets 0700 on it, reads the mode back, and
// removes it — answering nil when it could not do that at all.
//
// The question is about the FILESYSTEM and cannot be answered by asking the
// operating system, which is what made the check this feeds miss workers that
// are reachable today: /mnt/c on WSL2 is DrvFs over NTFS behind a perfectly
// ordinary GOOS=linux, and a root aimed at vfat or exfat (fmask/dmask
// synthesize a fixed mode), at CIFS without unix extensions, or at
// 9p/virtiofs under Lima answers the same way. It is the posture
// workspace_btrfs_linux.go already takes one package over: ask the filesystem
// what it is rather than assuming from the platform.
//
// Nil rather than false when the probe cannot run. False is REFUSED by the
// orchestrator, and a directory that cannot be written to is a different
// fact — one the upload immediately after reports far better than a
// handshake refusal blaming the executable bit would.
func execBitAt(dir string) *bool {
	return execBitVia(dir, func(name string) (fs.FileMode, error) {
		info, err := os.Stat(name)
		if err != nil {
			return 0, err //nolint:wrapcheck // the caller only asks whether there was an answer
		}

		return info.Mode().Perm(), nil
	})
}

// execBitVia is execBitAt with the read-back injected, because the answer
// turns entirely on reading the mode back and no filesystem a test can mount
// disagrees with the chmod that set it.
//
// Measured: a probe that trusted os.Chmod's own nil error, rather than what
// Stat says afterwards, passed every other test in this file — which is the
// same as having no test for the one line that does the work, on exactly the
// platforms the field exists for.
func execBitVia(dir string, stat func(string) (fs.FileMode, error)) *bool {
	probe := filepath.Join(dir, ".steps-execbit-probe")

	// Removed first, not only last: a probe left by a crashed shim would
	// otherwise be opened with its existing mode and answer about that
	// instead of about the filesystem.
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
	// platforms this exists for do not report one: os.Chmod on Windows
	// consults only the write bit and returns nil, and a mode-synthesizing
	// mount does the same. The mode afterwards is the only question they
	// answer honestly.
	mode, err := stat(probe)
	if err != nil {
		return nil
	}

	held := mode&0o111 != 0

	return &held
}
