package shim

// The cache is written on the PRODUCE path too (steps#138): what a fetch
// packs is filed under the digest the next step's offer will name.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// fetchAs is a fetch-all under a name, the shape a get's tree comes home in.
func (p *peer) fetchAs(artifact, dst string) {
	p.t.Helper()

	p.fetchWith(wire.Fetch{Artifact: artifact}, dst)
}

// TestFetchFilesWhatItPacked: a declared output fetched home is held by the
// worker afterwards, so offering it back — as the next step's input — costs
// nothing.
func TestFetchFilesWhatItPacked(t *testing.T) {
	root := t.TempDir()

	producer := newPeer(t, Options{Build: "test", Root: root})
	producer.hello()
	producer.exec("mkdir -p out/deep && echo made > out/deep/made.txt", nil)

	home := t.TempDir()
	producer.fetch([]string{"out"}, home)
	producer.goodbye()

	consumer := newPeer(t, Options{Build: "test", Root: root})
	consumer.hello()

	if answer := consumer.offerAnswer(home, "out"); answer != wire.FrameEnd {
		t.Errorf("the offer of what the worker itself produced was answered %v, want FrameEnd — the worker did not keep what it packed", answer)
	}
}

// TestFetchAllFilesTheTreeUnderItsName: a get's tree is the whole work
// directory, and it is filed under the name the next step will offer it by,
// not under the directory's own.
func TestFetchAllFilesTheTreeUnderItsName(t *testing.T) {
	root := t.TempDir()

	producer := newPeer(t, Options{Build: "test", Root: root})
	producer.hello()
	producer.exec("echo one > top.txt && mkdir nested && echo two > nested/deep.txt", nil)

	home := t.TempDir()
	producer.fetchAs("src", home)
	producer.goodbye()

	if got := mustRead(t, filepath.Join(home, "src", "nested", "deep.txt")); got != "two\n" {
		t.Fatalf("the tree came home as %q, want it under its artifact name", got)
	}

	consumer := newPeer(t, Options{Build: "test", Root: root})
	consumer.hello()

	if answer := consumer.offerAnswer(home, "src"); answer != wire.FrameEnd {
		t.Errorf("the offer of the fetched tree was answered %v, want FrameEnd", answer)
	}
}

// TestAVolatileWorkdirFilesNothingItProduced: on tmpfs the cache is memory,
// and a produced tree is optional — received ones still land, because the
// step needs them.
func TestAVolatileWorkdirFilesNothingItProduced(t *testing.T) {
	previous := fsInfo
	fsInfo = func(string) (string, uint64) { return "tmpfs", 1 << 30 }

	t.Cleanup(func() { fsInfo = previous })

	root := t.TempDir()

	producer := newPeer(t, Options{Build: "test", Root: root})
	producer.hello()
	producer.exec("mkdir out && echo made > out/made.txt", nil)

	home := t.TempDir()
	producer.fetch([]string{"out"}, home)
	producer.goodbye()

	entries, err := os.ReadDir(filepath.Join(root, "steps-shim", artifactCacheName))
	if err == nil && len(entries) > 0 {
		t.Errorf("the cache holds %d entries after a fetch on tmpfs, want none: %v", len(entries), entries)
	}

	consumer := newPeer(t, Options{Build: "test", Root: root})
	consumer.hello()

	if answer := consumer.offerAnswer(home, "out"); answer != wire.FrameNeed {
		t.Errorf("the offer was answered %v, want FrameNeed — a produced tree was filed in memory", answer)
	}
}
