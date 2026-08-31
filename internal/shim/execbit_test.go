package shim

// The shim answers the other question only it can: whether the filesystem the
// step's tree landed on can hold an executable bit.

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestShimProbesTheWorkdirItReports pins WHICH path the probe measures, for
// the reason its fsInfo sibling exists: on a machine with one filesystem,
// probing "/" instead of the workdir returns an identical answer, and the
// whole point of the field is that the workdir can be on a filesystem the
// rest of the machine is not.
func TestShimProbesTheWorkdirItReports(t *testing.T) {
	var asked []string

	lossy := false
	previous := execBit

	execBit = func(path string) *bool {
		asked = append(asked, path)

		return &lossy
	}

	t.Cleanup(func() { execBit = previous })

	peer := newPeer(t, Options{Root: t.TempDir(), Build: "test"})
	ok := peer.hello()

	if ok.ExecBit == nil || *ok.ExecBit {
		t.Errorf("HelloOK carried %v, want what the probe answered", ok.ExecBit)
	}

	if len(asked) != 1 {
		t.Fatalf("the probe was asked %d times, want once: %v", len(asked), asked)
	}

	if asked[0] != ok.Workdir {
		t.Errorf("probed %q but reported workdir %q — the answer describes the wrong directory", asked[0], ok.Workdir)
	}
}

// TestShimReportsAnOrdinaryWorkdirCanHoldAnExecutableBit runs the real
// syscalls, so the seam above cannot hide a probe that answers nothing.
//
// This is the positive half, and it is the only half any filesystem in this
// suite can produce: nothing mountable from a test can fail the probe, so the
// REFUSAL that the false answer earns is asserted one package over, against a
// shim that reports it (internal/venue's execbit_test.go). What is provable
// here is that an ordinary worker is not refused by a probe that mistakes its
// own bookkeeping for a filesystem fact.
func TestShimReportsAnOrdinaryWorkdirCanHoldAnExecutableBit(t *testing.T) {
	peer := newPeer(t, Options{Root: t.TempDir(), Build: "test"})

	ok := peer.hello()

	if ok.ExecBit == nil {
		t.Fatal("ExecBit is nil — the shim reported a workdir without saying whether it can hold 0111, and the orchestrator reads that as an older shim")
	}

	if !*ok.ExecBit {
		t.Errorf("ExecBit is false for %q, which would refuse every worker running this suite's own filesystem", ok.Workdir)
	}
}

// TestExecBitLeavesNothingBehind pins that the probe cleans up. It runs on
// every hello, against a directory a step's tree is about to be unpacked
// into and then DIGESTED — so a probe file that outlived its answer would
// change the hash of every tree that came home, which is the exact failure
// the probe exists to prevent.
func TestExecBitLeavesNothingBehind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	answer := execBitAt(dir)
	if answer == nil || !*answer {
		t.Fatalf("execBitAt(tempdir) = %v, want a measurement of true", answer)
	}

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(left) != 0 {
		t.Errorf("the probe left %d entries behind in the directory a tree is unpacked into and digested from", len(left))
	}
}

// TestExecBitIsSilentAboutWhatItCannotMeasure pins that a probe which could
// not run reports NOTHING rather than false.
//
// The distinction is the whole compatibility posture of this field: false is
// refused by the orchestrator, and a directory that cannot be written to is
// not a filesystem that strips modes — it is a different failure, which the
// upload immediately after says far better than a handshake refusal blaming
// the executable bit would.
func TestExecBitIsSilentAboutWhatItCannotMeasure(t *testing.T) {
	t.Parallel()

	if answer := execBitAt(filepath.Join(t.TempDir(), "no", "such", "place")); answer != nil {
		t.Errorf("execBitAt(missing) = %v, want no answer at all", *answer)
	}
}

// TestExecBitReadsTheModeBack is the assertion the rest of this file cannot
// make: every filesystem a test can reach agrees with the chmod it was just
// given, so a probe that reported success on os.Chmod's nil error rather than
// on the mode afterwards passes all of them — and fails only on the
// platforms the whole field exists for, silently, in production.
//
// Measured, not hypothesized: written as the sabotage of this fix, it
// survived.
func TestExecBitReadsTheModeBack(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		mode fs.FileMode
		err  error
		want *bool
	}{
		// The filesystem this exists for: the chmod is accepted, and the mode
		// that comes back is not the one that was set.
		{"a filesystem that synthesizes a fixed mode", 0o600, nil, boolp(false)},
		{"a filesystem that keeps what it was given", 0o700, nil, boolp(true)},
		// Any 0111 bit is enough — a mount that hands back 0755 for every
		// file is answering wrongly, but not in the direction that loses one.
		{"a filesystem that hands back more than was asked", 0o755, nil, boolp(true)},
		{"a directory that could not be read back at all", 0, os.ErrPermission, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			answer := execBitVia(t.TempDir(), func(string) (fs.FileMode, error) {
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

func boolp(v bool) *bool { return &v }
