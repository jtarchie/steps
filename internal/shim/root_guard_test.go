package shim

// The root a hello names is the peer's, and the peer is whoever reached the
// listener.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// TestHelloRefusesARootThatIsNotAnAbsolutePath is the sibling of the session
// name guard, on the other half of the same filepath.Join.
//
// Hello.Root is where the shim makes its scratch, sent because only the
// orchestrator knows the mapping — and therefore sent by whoever reached the
// listener, which is unauthenticated by design and root under the aws://
// bootstrap. A relative root resolves against the shim's working directory
// and a "../.." walks out of it, putting the scratch — and the RemoveAll that
// cleans it up — somewhere nobody named.
func TestHelloRefusesARootThatIsNotAnAbsolutePath(t *testing.T) {
	for _, root := range []string{
		"../..",
		"relative/path",
		"/tmp/../../etc",
		".",
	} {
		peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})

		op := peer.next()
		peer.send(wire.FrameHello, op, wire.Hello{
			Protocol: wire.Protocol, Build: "test",
			Session: "root-under-test", Root: root,
		})

		frame := peer.readAny()
		if frame.Type != wire.FrameError {
			t.Errorf("root %q was accepted (frame %v), want a refusal", root, frame.Type)

			continue
		}

		var reported wire.Error

		err := wire.DecodeJSON(frame, &reported)
		if err != nil {
			t.Fatalf("decoding the error: %v", err)
		}

		if !strings.Contains(reported.Message, "root") {
			t.Errorf("error for %q = %q, want it to name the root", root, reported.Message)
		}
	}
}

// TestAFailedHelloDoesNotOpenTheSession is the assignment order inside hello().
//
// A hello that fails only earns a FrameError — the run loop keeps the session
// alive — and every handshake gate is `s.workdir == ""`, so setting the path
// before the MkdirAll that has to succeed OPENED upload, exec, fetch and the
// raw root-docker proxy on a session whose scratch was never made. errReopened
// then refused the corrected hello that would have fixed it, and cleanup
// removed a path this session never created.
//
// Reached with a root checkRoot accepts on purpose: it validates the SHAPE of
// the path, and a well-shaped absolute path can still name a file.
func TestAFailedHelloDoesNotOpenTheSession(t *testing.T) {
	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})

	op := peer.next()
	peer.send(wire.FrameHello, op, wire.Hello{
		Protocol: wire.Protocol, Build: "test",
		Session: "failed-hello-under-test", Root: "/dev/null/x",
	})

	frame := peer.readAny()
	if frame.Type != wire.FrameError {
		t.Fatalf("frame type = %v, want an error for a root the shim cannot make", frame.Type)
	}

	op = peer.next()
	peer.send(wire.FrameExec, op, wire.Exec{Command: "echo unreachable"})

	frame = peer.readAny()
	if frame.Type != wire.FrameError {
		t.Fatalf("frame type = %v after a failed hello, want the operation refused rather than run", frame.Type)
	}

	var reported wire.Error

	err := wire.DecodeJSON(frame, &reported)
	if err != nil {
		t.Fatalf("decoding the error: %v", err)
	}

	if !strings.Contains(reported.Message, "no session") {
		t.Errorf("exec after a failed hello reported %q, want the unopened-session refusal", reported.Message)
	}

	// And the session is not wedged: the orchestrator can correct the root it
	// named and carry on.
	op = peer.next()
	peer.send(wire.FrameHello, op, wire.Hello{
		Protocol: wire.Protocol, Build: "test",
		Session: "failed-hello-under-test", Root: t.TempDir(),
	})

	var ok wire.HelloOK

	err = wire.DecodeJSON(peer.read(), &ok)
	if err != nil {
		t.Fatalf("decoding the corrected hello: %v", err)
	}

	if ok.Workdir == "" {
		t.Error("the corrected hello reported no work directory")
	}
}

// TestHelloAcceptsAnAbsoluteRoot pins that the guard does not break the
// feature it protects: the worker URL's path is how an operator names a disk
// on the worker, and that is an ordinary absolute path.
func TestHelloAcceptsAnAbsoluteRoot(t *testing.T) {
	root := t.TempDir()

	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})
	op := peer.next()

	peer.send(wire.FrameHello, op, wire.Hello{
		Protocol: wire.Protocol, Build: "test",
		Session: "root-under-test", Root: root,
	})

	var ok wire.HelloOK

	err := wire.DecodeJSON(peer.read(), &ok)
	if err != nil {
		t.Fatalf("decoding the hello answer: %v", err)
	}

	if !strings.HasPrefix(ok.Workdir, filepath.Clean(root)) {
		t.Errorf("workdir = %q, want it under the root the mapping named (%q)", ok.Workdir, root)
	}
}
