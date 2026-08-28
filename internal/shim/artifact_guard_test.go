package shim

// The names a manifest carries are the peer's, and the peer is whoever
// reached the listener.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// TestUploadRefusesAnArtifactPathThatLeavesItsTree is checkSessionName's rule
// on the two fields that arrived later.
//
// Name and Digest are joined onto the work directory and the artifact cache,
// so a separator or a ".." in either walks out of both — and the tar codec's
// own os.Root sandbox cannot help, because these come from the manifest
// rather than from a tar header. Both are single names by construction: the
// venue packs the TOP-LEVEL entries of a step's tree and keys them by a hash.
func TestUploadRefusesAnArtifactPathThatLeavesItsTree(t *testing.T) {
	for _, artifact := range []wire.UploadArtifact{
		{Name: "../escaped", Digest: "abc"},
		{Name: "nested/entry", Digest: "abc"},
		{Name: "/absolute", Digest: "abc"},
		{Name: "..", Digest: "abc"},
		{Name: "out", Digest: "../../escaped"},
		{Name: "out", Digest: "sub/dir"},
		{Name: "out", Digest: "/absolute"},
		{Name: "out", Digest: "."},
	} {
		peer := newPeer(t, Options{Build: "test"})
		peer.hello()

		op := peer.next()
		peer.send(wire.FrameUpload, op, wire.Upload{Artifacts: []wire.UploadArtifact{artifact}})

		answer := peer.readAny()
		if answer.Type != wire.FrameError {
			t.Errorf("artifact %+v was accepted (frame %v), want a refusal", artifact, answer.Type)
		}
	}
}

// TestPlaceArtifactRefusesAPathThatLeavesItsTree is the same rule on the
// store plane, which reaches the filesystem through its own function.
func TestPlaceArtifactRefusesAPathThatLeavesItsTree(t *testing.T) {
	session := &session{workdir: t.TempDir()}
	cache := t.TempDir()

	for _, artifact := range []wire.UploadArtifact{
		{Name: "../escaped", Digest: "abc", URL: "http://example.invalid"},
		{Name: "out", Digest: "../escaped", URL: "http://example.invalid"},
	} {
		err := session.placeArtifact(t.Context(), cache, artifact)
		if err == nil {
			t.Errorf("artifact %+v was accepted, want a refusal", artifact)
		}
	}
}

// TestPlacingAnArtifactDoesNotWriteThroughAPlantedLink is the second half of
// the same boundary: a name that is a single component still names a path
// somebody may already have made a symlink.
//
// The cache holds trees an earlier artifact wrote, and this codec recreates a
// symlink verbatim — so an artifact whose tree root is a link to somewhere
// outside puts that link in the work directory, and the NEXT artifact of the
// same name is written through it unless the copy is rooted.
func TestPlacingAnArtifactDoesNotWriteThroughAPlantedLink(t *testing.T) {
	base := t.TempDir()
	workdir := filepath.Join(base, "work")
	held := filepath.Join(base, "held")
	outside := filepath.Join(base, "outside")

	for _, dir := range []string{workdir, filepath.Join(held, "out"), outside} {
		err := os.MkdirAll(dir, 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	secret := filepath.Join(outside, "secret")

	err := os.WriteFile(secret, []byte("original"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	// What an earlier artifact of the same name would have left behind.
	err = os.Symlink(outside, filepath.Join(workdir, "out"))
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(held, "out", "secret"), []byte("planted"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_ = placeHeldArtifact(held, "out", workdir)

	after, err := os.ReadFile(secret) //nolint:gosec // a path this test made
	if err != nil {
		t.Fatalf("reading the file outside the work directory: %v", err)
	}

	if string(after) != "original" {
		t.Errorf("a planted link was followed: %q outside the work directory now reads %q", secret, after)
	}
}

// TestPlacingAnArtifactDoesNotWriteThroughALinkInsideTheTree is the half
// os.Root cannot refuse: a link whose target is inside the root is one it
// follows, so an artifact could still overwrite a sibling the step wrote.
func TestPlacingAnArtifactDoesNotWriteThroughALinkInsideTheTree(t *testing.T) {
	base := t.TempDir()
	workdir := filepath.Join(base, "work")
	held := filepath.Join(base, "held")

	err := os.MkdirAll(held, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.MkdirAll(filepath.Join(workdir, "kept"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	sibling := filepath.Join(workdir, "kept", "file")

	err = os.WriteFile(sibling, []byte("original"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink(filepath.Join("kept", "file"), filepath.Join(workdir, "out"))
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(held, "out"), []byte("planted"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_ = placeHeldArtifact(held, "out", workdir)

	after, err := os.ReadFile(sibling) //nolint:gosec // a path this test made
	if err != nil {
		t.Fatalf("reading the sibling: %v", err)
	}

	if string(after) != "original" {
		t.Errorf("a link inside the tree was followed: %q now reads %q", sibling, after)
	}
}

// TestPlacingAnArtifactRestoresAReadOnlyDirectory is the mode-staging rule
// the tar codec already keeps, on the copy that stands in for it.
//
// A recorded 0500 applied on the way past refuses its own children, so a
// cache HIT failed where the first transfer of the same tree succeeded.
func TestPlacingAnArtifactRestoresAReadOnlyDirectory(t *testing.T) {
	base := t.TempDir()
	workdir := filepath.Join(base, "work")
	held := filepath.Join(base, "held")
	locked := filepath.Join(held, "out", "locked")

	err := os.MkdirAll(locked, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.MkdirAll(workdir, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(locked, "file"), []byte("content"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Chmod(locked, 0o500) //nolint:gosec // the read-only directory IS the case under test
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // so t.TempDir can remove it

	err = placeHeldArtifact(held, "out", workdir)
	if err != nil {
		t.Fatalf("placing a tree with a read-only directory: %v", err)
	}

	placed := filepath.Join(workdir, "out", "locked")

	t.Cleanup(func() { _ = os.Chmod(placed, 0o700) }) //nolint:gosec // so t.TempDir can remove it

	content, err := os.ReadFile(filepath.Join(placed, "file")) //nolint:gosec // a path this test made
	if err != nil {
		t.Fatalf("reading the placed file: %v", err)
	}

	if string(content) != "content" {
		t.Errorf("placed file reads %q, want %q", content, "content")
	}

	info, err := os.Stat(placed)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o500 {
		t.Errorf("placed directory has mode %v, want 0500", info.Mode().Perm())
	}
}
