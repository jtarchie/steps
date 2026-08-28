//go:build unix

package shim

// The mode a placed artifact lands with, which is the umask's unless something
// puts it back.

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestPlacingAnArtifactKeepsItsFileModes is wire.UnpackTree's chmod, on the
// copy that stands in for it.
//
// OpenFile's mode argument is MASKED by the extracting process's umask, so a
// 0755 tool placed from the cache came out 0700 under a tight one — while the
// same tree arriving as tar kept its bits, making a cache HIT differ from the
// transfer it replaces. PackPaths then ships what is here back as the step's
// outputs, so the loss follows the artifact home.
func TestPlacingAnArtifactKeepsItsFileModes(t *testing.T) {
	base := t.TempDir()
	workdir := filepath.Join(base, "work")
	held := filepath.Join(base, "held")

	mustMkdir(t, workdir)
	mustMkdir(t, filepath.Join(held, "out"))

	tool := filepath.Join(held, "out", "tool")
	mustWrite(t, tool, "#!/bin/sh\n")

	err := os.Chmod(tool, 0o755) //nolint:gosec // an executable's mode IS the case under test
	if err != nil {
		t.Fatal(err)
	}

	// Set here rather than inherited: the default umask already strips a group
	// write bit, but 0077 is what makes the mask visible on a mode nobody's
	// umask leaves alone. Not parallel, because the umask is the process's.
	previous := syscall.Umask(0o077)
	defer syscall.Umask(previous)

	err = placeHeldArtifact(held, "out", workdir)
	if err != nil {
		t.Fatalf("placing the artifact: %v", err)
	}

	info, err := os.Stat(filepath.Join(workdir, "out", "tool"))
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o755 {
		t.Errorf("placed file has mode %v, want 0755 — the umask masked the mode the cache recorded", info.Mode().Perm())
	}
}
