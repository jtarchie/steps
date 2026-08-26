package venue

// What the worker's workdir sits on, carried across the boundary and reported.

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// TestVenueLearnsWhatTheWorkerWorkdirSitsOn crosses the seam: the shim
// statfs's a disk on the far end, and the value has to survive the handshake
// into the session. A field reported and then dropped here is the shape of bug
// this repo keeps finding — everything works, and the answer is never used.
func TestVenueLearnsWhatTheWorkerWorkdirSitsOn(t *testing.T) {
	t.Parallel()

	placed := newLocalRunner(t, localWorker(t, t.TempDir()))

	// The handshake is lazy, so nothing is known until a command forces it.
	err := placed.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	remote, ok := placed.(runner)
	if !ok {
		t.Fatalf("the local runner is %T, not a placed one", placed)
	}

	session := remote.session

	if session.fstype == "" {
		t.Error("the session learned no filesystem — the shim's answer was dropped in the handshake")
	}

	if session.fsfree == 0 {
		t.Error("the session learned no free space")
	}
}

// TestVolatileWorkdirIsReported pins which filesystems earn a warning and what
// it says. tmpfs is the default /tmp on Amazon Linux 2023, Fedora and recent
// Debian and Ubuntu, so an aws:// worker with no path in its URL lands there
// by default — spending the machine's memory on the binary and the tree, and
// losing the cached binary on every stop/start.
func TestVolatileWorkdirIsReported(t *testing.T) {
	t.Parallel()

	worker, err := ParseWorker("aws://i-0abc123def456789")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	for _, fstype := range []string{"tmpfs", "ramfs"} {
		notice := volatileWorkdirNotice(worker, wire.HelloOK{
			Workdir: "/tmp/steps-shim/run/work", FSType: fstype, FSFree: 966 << 20,
		})

		if notice == "" {
			t.Errorf("%s earned no warning, and it is memory", fstype)

			continue
		}

		for _, want := range []string{fstype, "/tmp/steps-shim/run/work", "966"} {
			if !strings.Contains(notice, want) {
				t.Errorf("notice for %s = %q, want it to name %q", fstype, notice, want)
			}
		}
	}

	// Everything else is a disk, including a filesystem this shim could only
	// name by its magic, and an older shim that said nothing at all. Warning
	// on silence would train operators to ignore the warning.
	for _, fstype := range []string{"btrfs", "ext", "xfs", "0x2fc12fc1", ""} {
		notice := volatileWorkdirNotice(worker, wire.HelloOK{Workdir: "/mnt/fast", FSType: fstype})
		if notice != "" {
			t.Errorf("fstype %q earned a warning it should not have: %q", fstype, notice)
		}
	}
}
