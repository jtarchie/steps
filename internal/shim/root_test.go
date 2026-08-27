package shim

// Where the step's scratch lands on the worker.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// TestShimPutsTheScratchWhereTheHelloSaid is what makes a worker URL's path
// mean anything.
//
// ssh://box/mnt/fast is an operator naming a disk on that machine — the one
// with room for a build tree, or the one that is not the root filesystem. The
// path reached remoteShimPath, so the pushed BINARY honoured it, but nothing
// carried it into the handshake: wire.Hello had no field for it and the shim
// read only its own Options. So the tree, which is the part with the size,
// went to the worker's temp directory regardless, and the mapping quietly did
// half of what it said.
func TestShimPutsTheScratchWhereTheHelloSaid(t *testing.T) {
	t.Parallel()

	named := t.TempDir()

	// A non-empty Options.Root as well, so this cannot pass by both being
	// unset: the hello has to WIN, not merely be consulted.
	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})

	op := peer.next()
	peer.send(wire.FrameHello, op, wire.Hello{
		Protocol: wire.Protocol,
		Build:    "test",
		Session:  "session-under-test",
		Root:     named,
	})

	var ok wire.HelloOK

	err := wire.DecodeJSON(peer.read(), &ok)
	if err != nil {
		t.Fatalf("decoding the hello answer: %v", err)
	}

	if !strings.HasPrefix(ok.Workdir, named) {
		t.Errorf("workdir = %q, want it under the root the orchestrator named (%q) — the worker URL's path chose a disk and the tree went somewhere else",
			ok.Workdir, named)
	}
}

// TestShimFallsBackToItsOwnRoot pins that a hello without a root still lands
// somewhere deliberate — the shape a shim started by hand takes.
func TestShimFallsBackToItsOwnRoot(t *testing.T) {
	t.Parallel()

	own := t.TempDir()
	peer := newPeer(t, Options{Build: "test", Root: own})

	if ok := peer.hello(); !strings.HasPrefix(ok.Workdir, own) {
		t.Errorf("workdir = %q, want it under the shim's own root %q", ok.Workdir, own)
	}
}

// TestHelloRefusesAnEscapingSessionName is the trust-boundary check on the one
// peer-supplied string that becomes a path: the session name is joined into
// the scratch directory, and cleanup removes that directory's PARENT — so
// "../.." made the shim delete a tree outside the root it was handed, as root
// under the aws:// bootstrap, driven by anything that can reach the
// deliberately unauthenticated --listen socket.
func TestHelloRefusesAnEscapingSessionName(t *testing.T) {
	root := t.TempDir()

	// A sibling of the root, holding a file, is what a traversal reaches:
	// filepath.Join(root, "steps-shim", "../..", "work") cleans to
	// <parent>/work, whose parent is the directory this lives in.
	victim := filepath.Join(filepath.Dir(root), "victim-"+filepath.Base(root))
	err := os.MkdirAll(victim, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(victim) })

	keep := filepath.Join(victim, "keep.txt")
	err = os.WriteFile(keep, []byte("do not delete me"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"../..", "..", ".", "", "a/b", "/etc"} {
		t.Run(name, func(t *testing.T) {
			peer := newPeer(t, Options{Build: "test", Root: root})

			op := peer.next()
			peer.send(wire.FrameHello, op, wire.Hello{Protocol: wire.Protocol, Build: "test", Session: name})

			frame, err := peer.decoder.Read()
			if err != nil {
				t.Fatalf("reading the shim's answer: %v", err)
			}

			if frame.Type != wire.FrameError {
				t.Fatalf("the shim answered a type %d frame for session %q, want a refusal", frame.Type, name)
			}
		})
	}

	_, err = os.Stat(keep)
	if err != nil {
		t.Fatalf("the shim deleted a tree outside its root: %v", err)
	}
}

// TestSecondHelloIsRefused pins the other half of the hello's trust boundary.
//
// hello() writes the workdir, the keep flag and both negotiated tokens, and a
// command goroutine started by startExec reads the workdir while the frame loop
// keeps running — so a second hello rewrote state under a running command, and
// pointed cleanup's RemoveAll at a directory this session never made.
func TestSecondHelloIsRefused(t *testing.T) {
	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})

	first := peer.hello()
	if first.Workdir == "" {
		t.Fatal("the first hello reported no workdir")
	}

	op := peer.next()
	peer.send(wire.FrameHello, op, wire.Hello{Protocol: wire.Protocol, Build: "test", Session: "a-second-session"})

	frame, err := peer.decoder.Read()
	if err != nil {
		t.Fatalf("reading the shim's answer: %v", err)
	}

	if frame.Type != wire.FrameError {
		t.Fatalf("the shim answered a type %d frame to a second hello, want a refusal", frame.Type)
	}

	// And the session still points where the first hello put it.
	stdout, _, exit := peer.exec("pwd", nil)
	if !exit.Started || exit.Code != 0 {
		t.Fatalf("exit = %+v, want the session still usable", exit)
	}

	// By the session directory rather than the whole path: macOS resolves
	// /var to /private/var under the command, and what is being pinned is
	// which SESSION the workdir belongs to.
	if !strings.Contains(stdout, "/steps-shim/session-under-test/work") {
		t.Errorf("the command ran in %q, want the first hello's session directory", strings.TrimSpace(stdout))
	}
}
