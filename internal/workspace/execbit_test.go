package workspace

// workspace.root: is an operator-settable directory that trees are unpacked
// into and DIGESTED from, which is the same silent loss the venue refuses a
// worker for — available here without any worker involved.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestCopyProviderRefusesARootThatCannotHoldAnExecutableBit is the reason the
// probe exists, asserted where an operator meets it: startup.
//
// A root on vfat/exfat (fmask/dmask synthesize a fixed mode), on CIFS without
// unix extensions, or on 9p/virtiofs under Lima or Docker Desktop takes every
// chmod without complaint and hands back a mode nobody set. digestTree hashes
// the executable bit as content, so a tree materialized there and digested
// again is content that changed for no reason a reader can point at — and the
// step cache cannot tell a stripped bit from an edit.
//
// Driven through Validate rather than the probe, because the bug shape being
// guarded against is a probe that is never consulted.
func TestCopyProviderRefusesARootThatCannotHoldAnExecutableBit(t *testing.T) {
	root := t.TempDir()

	previous := rootExecBit
	rootExecBit = func(string) *bool { no := false; return &no }

	t.Cleanup(func() { rootExecBit = previous })

	provider, err := newCopyProvider(&config.WorkspaceConfig{Root: root}, false) //nolint:exhaustruct // Root is the field under test
	if err != nil {
		t.Fatal(err)
	}

	err = provider.Validate()
	if err == nil {
		t.Fatal("a workspace root that cannot hold an executable bit was accepted, and every tree digested under it would silently lose one")
	}

	// The root, because that is the thing to act on: the answer is to point
	// workspace.root: somewhere else.
	if !strings.Contains(err.Error(), root) {
		t.Errorf("error = %v, want it to name the root that was refused", err)
	}
}

// TestCopyProviderAcceptsAnOrdinaryRoot is the other half, and the one that
// catches a probe that refuses everything: every filesystem this suite runs
// on holds the bit, so the check must be invisible on all of them.
func TestCopyProviderAcceptsAnOrdinaryRoot(t *testing.T) {
	t.Parallel()

	provider, err := newCopyProvider(&config.WorkspaceConfig{Root: t.TempDir()}, false) //nolint:exhaustruct // Root is the field under test
	if err != nil {
		t.Fatal(err)
	}

	err = provider.Validate()
	if err != nil {
		t.Fatalf("an ordinary workspace root was refused: %v", err)
	}
}

// TestRootExecBitReadsTheModeBack is the same assertion internal/shim makes
// about its own copy of this probe, and for the same reason: no filesystem a
// test can mount disagrees with the chmod it was just given, so a probe that
// trusted os.Chmod's nil error would pass every other test here while failing
// only on the filesystems the check exists for.
func TestRootExecBitReadsTheModeBack(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		mode fs.FileMode
		err  error
		want *bool
	}{
		{"a filesystem that synthesizes a fixed mode", 0o600, nil, boolp(false)},
		{"a filesystem that keeps what it was given", 0o700, nil, boolp(true)},
		{"a root that could not be read back at all", 0, os.ErrPermission, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			answer := rootExecBitVia(t.TempDir(), func(string) (fs.FileMode, error) {
				return tc.mode, tc.err
			})

			switch {
			case tc.want == nil && answer != nil:
				t.Errorf("answered %v, want no answer at all", *answer)
			case tc.want != nil && answer == nil:
				t.Errorf("answered nothing, want %v", *tc.want)
			case tc.want != nil && answer != nil && *answer != *tc.want:
				t.Errorf("answered %v, want %v", *answer, *tc.want)
			}
		})
	}
}

// TestRootExecBitLeavesNothingBehind pins the cleanup. The directory it
// probes is the one build trees are materialized into and digested from, so a
// probe file that outlived its answer would change the very hashes the probe
// is protecting.
func TestRootExecBitLeavesNothingBehind(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	if answer := rootExecBitAt(root); answer == nil || !*answer {
		t.Fatalf("rootExecBitAt(tempdir) = %v, want a measurement of true", answer)
	}

	left, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	if len(left) != 0 {
		t.Errorf("the probe left %d entries behind in the workspace root", len(left))
	}
}

// TestRootExecBitIsSilentAboutWhatItCannotMeasure runs the real probe against
// a directory that is not there, pinning that an unmeasurable root answers
// NOTHING rather than false.
//
// The distinction decides whether Validate refuses: a root that cannot be
// written to is a different failure, and validateRootWritable beside this one
// reports it far better than a refusal blaming the executable bit would.
func TestRootExecBitIsSilentAboutWhatItCannotMeasure(t *testing.T) {
	t.Parallel()

	if answer := rootExecBitAt(filepath.Join(t.TempDir(), "no", "such", "place")); answer != nil {
		t.Errorf("rootExecBitAt(missing) = %v, want no answer at all", *answer)
	}
}

func boolp(v bool) *bool { return &v }
