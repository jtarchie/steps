package shim

// Where the step's scratch lands on the worker.

import (
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
