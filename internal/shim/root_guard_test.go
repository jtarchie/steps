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
		peer := newPeer(t, Options{Build: "test"})

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

// TestHelloAcceptsAnAbsoluteRoot pins that the guard does not break the
// feature it protects: the worker URL's path is how an operator names a disk
// on the worker, and that is an ordinary absolute path.
func TestHelloAcceptsAnAbsoluteRoot(t *testing.T) {
	root := t.TempDir()

	peer := newPeer(t, Options{Build: "test"})
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
