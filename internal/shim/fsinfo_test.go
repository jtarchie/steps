package shim

// The shim answers a question only it can: what filesystem the step's tree
// actually landed on.

import (
	"runtime"
	"testing"
)

// TestShimMeasuresTheWorkdirItReports pins WHICH path the hello measures.
//
// Not provable by reading the answer: on a machine with one filesystem —
// this repo's own dev machine, where / and $TMPDIR are the same APFS volume —
// measuring "/" instead of the workdir returns a byte-identical result. The
// seam is the only thing that can tell those two apart, which is why it
// exists.
func TestShimMeasuresTheWorkdirItReports(t *testing.T) {
	var asked []string

	previous := fsInfo
	fsInfo = func(path string) (string, uint64) {
		asked = append(asked, path)

		return "recorded", 1 << 30
	}

	t.Cleanup(func() { fsInfo = previous })

	peer := newPeer(t, Options{Root: t.TempDir(), Build: "test"})
	ok := peer.hello()

	if ok.FSType != "recorded" || ok.FSFree != 1<<30 {
		t.Errorf("HelloOK carried %q/%d, want what fsInfo answered", ok.FSType, ok.FSFree)
	}

	if len(asked) != 1 {
		t.Fatalf("fsInfo was asked %d times, want once: %v", len(asked), asked)
	}

	if asked[0] != ok.Workdir {
		t.Errorf("measured %q but reported workdir %q — the answer describes the wrong disk", asked[0], ok.Workdir)
	}
}

// TestShimReportsTheFilesystemUnderItsWorkdir runs the real syscall, so the
// seam above cannot hide an implementation that answers nothing.
//
// The orchestrator cannot statfs a disk on another machine, so silence here
// is indistinguishable from "no idea" — and the two questions this answers,
// whether a workdir is memory and whether it is btrfs, both fail closed on
// silence rather than guessing.
func TestShimReportsTheFilesystemUnderItsWorkdir(t *testing.T) {
	peer := newPeer(t, Options{Root: t.TempDir(), Build: "test"})

	ok := peer.hello()

	if ok.FSType == "" {
		t.Error("FSType is empty — the shim reported a workdir without saying what it sits on")
	}

	if ok.FSFree == 0 {
		t.Error("FSFree is zero — a writable temp dir with no space available is not credible")
	}

	if ok.Workdir == "" {
		t.Error("Workdir is empty")
	}
}

// TestFSInfoReadsThePathItIsGiven proves the real implementation looks at its
// argument rather than answering for the process, using the one pair of
// distinct filesystems every machine of each kind actually has.
func TestFSInfoReadsThePathItIsGiven(t *testing.T) {
	t.Parallel()

	var elsewhere string

	switch runtime.GOOS {
	case "darwin":
		elsewhere = "/dev" // devfs, never the volume a temp dir is on
	case "linux":
		elsewhere = "/proc" // procfs, likewise
	default:
		t.Skipf("no known second filesystem on %s", runtime.GOOS)
	}

	here, _ := fsInfoAt(t.TempDir())
	there, _ := fsInfoAt(elsewhere)

	if here == "" || there == "" {
		t.Fatalf("fsInfoAt gave no answer: temp %q, %s %q", here, elsewhere, there)
	}

	if here == there {
		t.Errorf("fsInfoAt(tempdir) and fsInfoAt(%s) both said %q — it is not reading the path", elsewhere, here)
	}
}

// TestFSInfoIsSilentAboutWhatDoesNotExist pins that a bad path is reported as
// unknown rather than as a fabricated filesystem.
func TestFSInfoIsSilentAboutWhatDoesNotExist(t *testing.T) {
	t.Parallel()

	name, free := fsInfoAt(t.TempDir() + "/no/such/place")
	if name != "" || free != 0 {
		t.Errorf("fsInfoAt(missing) = %q/%d, want unknown", name, free)
	}
}
