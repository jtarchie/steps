package shim

// A tree produced on another worker may not name files on this one.

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// escapingTree is a tree holding out/leak -> /etc/hosts.
func escapingTree(t *testing.T) string {
	t.Helper()

	src := t.TempDir()

	err := os.MkdirAll(filepath.Join(src, "out"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink("/etc/hosts", filepath.Join(src, "out", "leak"))
	if err != nil {
		t.Fatal(err)
	}

	return src
}

// offerOnTunnel offers src's out on the tunnel as foreign or not, sends it,
// and answers the shim's reply.
func offerOnTunnel(t *testing.T, root, src string, foreign bool) wire.Frame {
	t.Helper()

	peer := newPeer(t, Options{Build: "test", Root: root})
	peer.hello()

	op := peer.next()
	peer.send(wire.FrameUpload, op, wire.Upload{
		Artifacts: []wire.UploadArtifact{{Name: "out", Digest: artifactDigest(t, src, "out"), Foreign: foreign}},
	})

	if answer := peer.read(); answer.Type != wire.FrameNeed {
		t.Fatalf("expected the shim to ask for the artifact, got a type %d frame", answer.Type)
	}

	writer := peer.dataWriter(op)

	err := wire.PackPaths(writer, src, []string{"out"})
	if err != nil {
		t.Fatal(err)
	}

	err = writer.flush()
	if err != nil {
		t.Fatal(err)
	}

	peer.sendEmpty(wire.FrameEnd, op)

	return peer.readAny()
}

// TestAForeignOfferRefusesAnEscapingLink: a tree another worker produced
// carries links whose targets name paths on THIS worker, and the step here
// would read them — the check the orchestrator ran when every such tree
// landed on it first.
func TestAForeignOfferRefusesAnEscapingLink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	src := escapingTree(t)

	frame := offerOnTunnel(t, root, src, true)
	if frame.Type != wire.FrameError {
		t.Fatalf("the shim accepted a foreign tree with an escaping link: type %d frame", frame.Type)
	}

	var refusal wire.Error

	_ = wire.DecodeJSON(frame, &refusal)

	if !strings.Contains(refusal.Message, "points outside the tree") {
		t.Errorf("refusal reads %q, want it to name the link", refusal.Message)
	}

	if placed := findLink(t, root, "leak"); placed != "" {
		t.Errorf("the link was placed at %s", placed)
	}

	_, err := os.Stat(filepath.Join(root, "steps-shim", artifactCacheName, artifactDigest(t, src, "out")))
	if err == nil {
		t.Error("the foreign tree was cached under its digest")
	}
}

// TestAnOwnOfferKeepsAnEscapingLink pins the other half: the orchestrator's
// own trees round-trip verbatim, links and all.
func TestAnOwnOfferKeepsAnEscapingLink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	frame := offerOnTunnel(t, root, escapingTree(t), false)
	if frame.Type != wire.FrameEnd {
		t.Fatalf("the shim refused the orchestrator's own tree: type %d frame", frame.Type)
	}

	if placed := findLink(t, root, "leak"); placed == "" {
		t.Error("the link was not placed")
	}
}

// TestAForeignStoreObjectRefusesAnEscapingLink is the same refusal on the
// URL plane, where a tree another worker pushed arrives from the store.
func TestAForeignStoreObjectRefusesAnEscapingLink(t *testing.T) {
	t.Parallel()

	src := escapingTree(t)

	server := httptest.NewServer(&blobHost{tree: packTree(t, src, "out")})
	t.Cleanup(server.Close)

	for _, foreign := range []bool{true, false} {
		cache := t.TempDir()
		session := &session{workdir: t.TempDir()}

		err := session.placeArtifact(t.Context(), cache, wire.UploadArtifact{
			Name: "out", Digest: artifactDigest(t, src, "out"), URL: server.URL + "/wire/tree", Foreign: foreign,
		})

		_, statErr := os.Lstat(filepath.Join(session.workdir, "out", "leak"))

		if !foreign {
			if err != nil || statErr != nil {
				t.Errorf("an own tree was not placed verbatim: %v, %v", err, statErr)
			}

			continue
		}

		if err == nil || !strings.Contains(err.Error(), "points outside the tree") {
			t.Errorf("a foreign store object with an escaping link was accepted: %v", err)
		}

		if statErr == nil {
			t.Error("the foreign link was placed")
		}
	}
}

// findLink answers where a symlink named name sits under root, or empty.
func findLink(t *testing.T, root, name string) string {
	t.Helper()

	var found string

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode()&os.ModeSymlink != 0 && filepath.Base(path) == name {
			found = path
		}

		return nil
	})

	return found
}
