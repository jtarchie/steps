package workspace

// Does a tree digest the same on a worker whose filesystem has no executable
// bit as it does here? This is the experiment #83 asks for, mechanized.
//
// It cannot be run ON Windows from this repo — there is no CI, and a venue
// today runs the orchestrator's OWN binary (internal/venue's writeRemote
// uploads the running executable), so a Windows worker is not reachable at
// all. What CAN be done is to model exactly what Windows does to a mode, from
// the Go source rather than from memory, and put a real tree through it:
//
//   - os.Chmod on Windows toggles one attribute. syscall.Chmod
//     (syscall/syscall_windows.go) reads mode&S_IWRITE and sets or clears
//     FILE_ATTRIBUTE_READONLY; 0111 is not consulted and no error is
//     returned. So UnpackTree's chmod SUCCEEDS and drops the executable bit.
//   - os.Stat on Windows synthesizes the mode it reports. fileStat.mode
//     (os/types_windows.go) yields 0444 for a read-only file and 0666
//     otherwise, and ORs in 0111 only for a directory. So a regular file on
//     Windows can never read back as executable, whatever was written.
//
// windowsModes below is those two facts applied to a directory, which makes
// this an honest simulation of the mode half of the question and of nothing
// else. The symlink half — Windows stores a link target in a reparse point
// and os.Readlink returns it with backslashes — is NOT modelled here, because
// a POSIX filesystem cannot hold a link the way NTFS does; see
// TestDigestTreeReadsALinkTargetAsAPath for the part of that which is
// decidable here.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// windowsModes rewrites a tree's permissions the way a Windows filesystem
// would report them back. Every regular file becomes 0666, or 0444 if nothing
// could write it — the only two modes os.Stat produces there.
//
// Directories are left alone: Windows reports ModeDir|0111 for them, and
// digestTree never reads a directory's mode (digestEntry writes only the kind
// byte for one), so modelling that would be modelling a difference nothing
// can observe.
func windowsModes(t *testing.T, root string) {
	t.Helper()

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if !entry.Type().IsRegular() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("%w", err)
		}

		windows := os.FileMode(0o666)
		if info.Mode().Perm()&0o200 == 0 {
			windows = 0o444
		}

		//nolint:gosec // a t.TempDir() this test built itself, entry by entry
		return os.Chmod(path, windows)
	})
	if err != nil {
		t.Fatalf("applying Windows modes to %q: %v", root, err)
	}
}

// TestAWorkerWithNoExecutableBitLosesItAcrossTheWire is the answer to #83, and
// it is worse than the cache miss the question anticipated.
//
// digestTree runs only on the orchestrator — the shim imports internal/wire
// and never internal/workspace — so a Windows worker never disagrees about a
// digest. What it does instead is silently REMOVE the executable bit from
// every tree that passes through it: unpack cannot set 0111, the repack on the
// way home reads 0666 off the filesystem and writes that into the tar, and the
// tree the orchestrator gets back is not the tree it sent. The step cache sees
// content that changed for no reason a reader could point at, and any later
// step that tries to run the file cannot.
//
// So the exposure is not "Windows misses the cache". It is "a script stops
// being a script by being sent somewhere", with nothing anywhere raising an
// error.
func TestAWorkerWithNoExecutableBitLosesItAcrossTheWire(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	write(t, home, "run.sh", "#!/bin/sh\necho hi\n", 0o700)
	write(t, home, "notes.txt", "plain\n", 0o600)

	sent := mustDigest(t, home)

	// Out to the worker, which stores what its filesystem can.
	worker := t.TempDir()
	roundTrip(t, home, worker)
	windowsModes(t, worker)

	// And back, unchanged by the step itself.
	returned := t.TempDir()
	roundTrip(t, worker, returned)

	if got := mustDigest(t, returned); got == sent {
		t.Fatal("the executable bit survived a filesystem that cannot store one — the model in windowsModes is wrong, or digestTree stopped reading the bit")
	}

	info, err := os.Stat(filepath.Join(returned, "run.sh"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if info.Mode()&0o111 != 0 {
		t.Errorf("run.sh came home as %v, want the executable bit gone — the loss is what this test documents", info.Mode())
	}
}

// TestOnlyTheExecutableBitDivergesOnSuchAWorker bounds the finding above, which
// is the half that decides how expensive the answer is. If several properties
// diverged, the fix would have to be a canonical mode per file class; if only
// one does, the fix is about one bit and can be argued on its own.
//
// Every shape the codec is already tested against goes through the same
// worker filesystem, and each one that contains no executable file must come
// home identical — including a 0600 file, whose restricted mode is invisible
// to the digest by design and must not become visible now.
//
// The symlink shapes pass here because a POSIX filesystem stores a link target
// verbatim. That is a fact about this simulation, NOT a verification of
// Windows: reparse-point storage is unmodelled and remains open.
func TestOnlyTheExecutableBitDivergesOnSuchAWorker(t *testing.T) {
	t.Parallel()

	for _, shape := range treeShapes() {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()

			home := t.TempDir()
			shape.build(t, home)

			worker := t.TempDir()
			roundTrip(t, home, worker)
			windowsModes(t, worker)

			returned := t.TempDir()
			roundTrip(t, worker, returned)

			same := mustDigest(t, returned) == mustDigest(t, home)
			if want := !hasExecutable(t, home); same != want {
				t.Errorf("digest preserved = %v, want %v: only a tree holding an executable file may diverge on a worker that cannot store the bit", same, want)
			}
		})
	}
}

// hasExecutable reports whether any regular file in the tree is executable —
// the one property the modelled worker cannot carry.
func hasExecutable(t *testing.T, root string) bool {
	t.Helper()

	found := false

	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if !entry.Type().IsRegular() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("%w", err)
		}

		if info.Mode()&0o111 != 0 {
			found = true
		}

		return nil
	})
	if err != nil {
		t.Fatalf("scanning %q: %v", root, err)
	}

	return found
}

// TestDigestTreeReadsALinkTargetAsAPath is the symlink half of #83, as far as
// it is decidable without a Windows machine.
//
// A link's target is a path, and digestTree already normalizes the other paths
// it hashes ("ToSlash so an artifact digested on one platform matches the same
// bytes digested on another") — but the target text was hashed raw, so the
// same logical link would hash one way where separators are slashes and
// another where they are backslashes.
//
// The normalization is free on this side: filepath.ToSlash is the identity
// wherever Separator is '/', so no existing digest moves. This test pins that
// identity, which is what makes the change safe to make blind — the assertion
// that matters on Windows cannot be run here, and a backslash is an ordinary
// filename character on POSIX, so the two cases have to be spelled apart.
func TestDigestTreeReadsALinkTargetAsAPath(t *testing.T) {
	t.Parallel()

	// The SAME link name in both trees, which is the whole discipline here:
	// digestTree length-prefixes each entry's relative path before its kind
	// and target, so two trees whose links are named differently hash
	// differently whatever the targets say — an assertion built that way
	// passes with the normalization present, absent, or replaced by anything
	// at all.
	const link = "pointer"

	slashed := t.TempDir()
	symlink(t, slashed, "a/b", link)

	backslashed := t.TempDir()
	symlink(t, backslashed, `a\b`, link)

	// On POSIX these are two different targets: a backslash is an ordinary
	// filename character, `a\b` names one component, and ToSlash is the
	// identity — so they must not collide. On Windows the same two trees are
	// one link written two ways, and the normalization is what makes them
	// agree. Only the first half is decidable here, and it is the half that
	// says no existing digest moved.
	if mustDigest(t, slashed) == mustDigest(t, backslashed) {
		t.Error("a separator and a literal backslash in a link target hashed alike")
	}

	// And the normalization must be the identity on this side, since every
	// cache entry in existence was written without it.
	again := t.TempDir()
	symlink(t, again, "a/b", link)

	if mustDigest(t, again) != mustDigest(t, slashed) {
		t.Error("the same link hashes differently in two trees")
	}

	target, err := os.Readlink(filepath.Join(backslashed, link))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}

	if !strings.Contains(target, `\`) {
		t.Fatalf("the platform rewrote a backslash in a link target (%q); this test cannot say what it pins", target)
	}
}
