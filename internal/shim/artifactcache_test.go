package shim

// One cache, every session on the worker: which makes both its NAME and its
// contents things another session can take away.

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/wire"
)

// artifactDigest is the digest the ORCHESTRATOR would send one artifact under:
// sha256 over the uncompressed tar stream, which is where packArtifactToFile
// tees its hasher.
//
// Computed rather than invented, because the shim now verifies it. A test that
// names content by a digest the content does not have is testing a message no
// venue can send, and the fiction was load-bearing here before — the cache was
// keyed on a hash of the artifact's NAME so the same artifact always repeated.
func artifactDigest(t *testing.T, src, name string) string {
	t.Helper()

	hasher := sha256.New()

	err := wire.PackPaths(hasher, src, []string{name})
	if err != nil {
		t.Fatalf("digesting artifact %q: %v", name, err)
	}

	return hex.EncodeToString(hasher.Sum(nil))
}

// TestHelloRefusesTheArtifactCacheAsASessionName is the collision between two
// names joined under one directory.
//
// The cache is a SIBLING of the per-session scratch, so a session called
// "artifacts" IS the shared cache: the sweep enumerates that session's live
// work directory as a cache entry, and its goodbye — cleanup removes the work
// directory's PARENT — takes every artifact every other session on the worker
// had cached. The listener is unauthenticated by design and root under the
// aws:// bootstrap, which is what checkSessionName exists for.
func TestHelloRefusesTheArtifactCacheAsASessionName(t *testing.T) {
	root := t.TempDir()

	benign := newPeer(t, Options{Build: "test", Root: root})
	benign.hello()

	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "data", "seed.txt"), "seed\n")
	benign.upload(src)

	cached := filepath.Join(root, "steps-shim", artifactCacheName, artifactDigest(t, src, "data"))

	_, err := os.Stat(cached)
	if err != nil {
		t.Fatalf("the benign session cached nothing to lose: %v", err)
	}

	evil := newPeer(t, Options{Build: "test", Root: root})

	op := evil.next()
	evil.send(wire.FrameHello, op, wire.Hello{
		Protocol: wire.Protocol, Build: "test", Session: artifactCacheName,
	})

	frame := evil.readAny()
	if frame.Type != wire.FrameError {
		t.Fatalf("frame type = %v, want a refusal: that session name IS the shared cache", frame.Type)
	}

	evil.sendEmpty(wire.FrameBye, evil.next())

	err = evil.wait()
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}

	_, err = os.Stat(cached)
	if err != nil {
		t.Errorf("the refused session's goodbye still took the shared cache with it: %v", err)
	}
}

// TestUploadRefetchesACacheEntryThatVanished is the sweep racing a read.
//
// sweepArtifactCache skips staging directories, which are unpacks in flight,
// and nothing else — so it can RemoveAll an entry another session is mid-copy
// out of, and sessions are genuinely concurrent both in this process and
// across the one shim PROCESS per placed step the aws:// bootstrap starts on a
// shared --root. A lock cannot cover that, so a cache entry that vanishes has
// to read as a MISS: the placement asks for the bytes instead of failing a
// step over another step's bookkeeping.
//
// The directory-with-no-tree is the shape a sweep really leaves: RemoveAll
// takes the children first, so the digest outlives its contents.
func TestUploadRefetchesACacheEntryThatVanished(t *testing.T) {
	root := t.TempDir()
	peer := newPeer(t, Options{Build: "test", Root: root})
	ok := peer.hello()

	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "data", "seed.txt"), "seed\n")

	mustMkdir(t, filepath.Join(root, "steps-shim", artifactCacheName, artifactDigest(t, src, "data")))

	// Fails the test if the shim answers an error rather than asking for the
	// bytes it no longer holds.
	peer.upload(src)

	got := mustRead(t, filepath.Join(ok.Workdir, "data", "seed.txt"))
	if got != "seed\n" {
		t.Errorf("placed file = %q, want %q", got, "seed\n")
	}
}

// TestPlaceArtifactRefetchesACacheEntryThatVanished is the same fall-through on
// the store plane, where the fetch is a GET rather than a FrameNeed.
func TestPlaceArtifactRefetchesACacheEntryThatVanished(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "data", "seed.txt"), "seed\n")

	server := httptest.NewServer(&blobHost{tree: packTree(t, src, "data")})
	t.Cleanup(server.Close)

	cache := t.TempDir()
	session := &session{workdir: t.TempDir()}

	digest := artifactDigest(t, src, "data")

	mustMkdir(t, filepath.Join(cache, digest))

	err := session.placeArtifact(t.Context(), cache, wire.UploadArtifact{
		Name: "data", Digest: digest, URL: server.URL + "/wire/tree",
	})
	if err != nil {
		t.Fatalf("placing an artifact whose cache entry vanished: %v", err)
	}

	got := mustRead(t, filepath.Join(session.workdir, "data", "seed.txt"))
	if got != "seed\n" {
		t.Errorf("placed file = %q, want %q", got, "seed\n")
	}
}

// shrinkArtifactCache makes the cap small enough for a test to cross with a
// few kilobytes.
func shrinkArtifactCache(t *testing.T, bound int64) {
	t.Helper()

	previous := artifactCacheBytes
	artifactCacheBytes = bound

	t.Cleanup(func() { artifactCacheBytes = previous })
}

// writeCacheEntry makes one cache entry of size bytes, with a modtime that
// decides how cold it is.
func writeCacheEntry(t *testing.T, cache, name string, size int, used time.Time) string {
	t.Helper()

	dir := filepath.Join(cache, name)

	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "blob"), make([]byte, size), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Chtimes(dir, used, used)
	if err != nil {
		t.Fatal(err)
	}

	return dir
}

// TestSweepCountsAbandonedStagingTrees pins the accounting half of the
// staging skip.
//
// Not evicting a .partial-* tree is right: it is another session's transfer in
// flight, and deleting it mid-unpack commits a truncated tree under a digest
// claiming to be whole. Not COUNTING it was the bug. A shim killed mid-unpack
// — OOM, spot reclamation, kill -9 — never runs the deferred RemoveAll that
// would clear it, session cleanup only removes <root>/steps-shim/<session>,
// and nothing else touches the cache. So the orphan sat there forever while
// the sweep reported the cache under its cap, and the cap stopped describing
// the disk.
func TestSweepCountsAbandonedStagingTrees(t *testing.T) {
	shrinkArtifactCache(t, 6<<10)

	cache := t.TempDir()
	old := time.Now().Add(-time.Hour)

	orphan := writeCacheEntry(t, cache, stagingPrefix+"dead", 5<<10, old)
	entry := writeCacheEntry(t, cache, "digest-a", 4<<10, old.Add(time.Minute))

	err := sweepArtifactCache(cache)
	if err != nil {
		t.Fatalf("sweepArtifactCache: %v", err)
	}

	_, err = os.Stat(orphan)
	if err != nil {
		t.Errorf("the staging tree was evicted: %v — a transfer in flight must survive the sweep", err)
	}

	_, err = os.Stat(entry)
	if !os.IsNotExist(err) {
		t.Errorf("the cache holds %d bytes against a %d cap and evicted nothing: the orphan's bytes were not counted",
			9<<10, artifactCacheBytes)
	}
}

// TestSweepKeepsCountingWhatItCouldNotDelete pins the other end of the same
// total.
//
// Subtracting an entry's bytes unconditionally let ONE undeletable entry end
// the loop having freed nothing: it is the coldest, so it is picked first
// every sweep, subtracted every sweep, and the cap stops bounding anything
// from then on. A directory the codec restored read-only is the ordinary way
// to get one — RemoveAll cannot enter it and does not chmod.
//
// What it leaves behind is a TOMBSTONE rather than a half-entry under the
// digest: evictArtifact renames first, so the entry is out of service even
// when the delete fails, and the next sweep counts the tombstone and tries
// again. See TestEvictionNeverLeavesAPartialEntryUnderItsDigest.
func TestSweepKeepsCountingWhatItCouldNotDelete(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can remove a read-only directory, so there is no undeletable entry to make")
	}

	shrinkArtifactCache(t, 6<<10)

	cache := t.TempDir()
	old := time.Now().Add(-time.Hour)

	stuck := writeCacheEntry(t, cache, "digest-stuck", 5<<10, old)
	next := writeCacheEntry(t, cache, "digest-next", 4<<10, old.Add(time.Minute))

	// Read-only, which is how RemoveAll gets an entry it cannot delete: it
	// never chmods, so it cannot enter the directory to unlink what is inside.
	err := os.Chmod(stuck, 0o500) //nolint:gosec // an undeletable entry is the condition under test
	if err != nil {
		t.Fatal(err)
	}

	// Unlocked by walking rather than by path: eviction renames before it
	// deletes, so by cleanup time this directory is under a tombstone name
	// and the path the test narrowed no longer exists.
	t.Cleanup(func() { unlockTree(cache) })

	err = sweepArtifactCache(cache)
	if err != nil {
		t.Fatalf("sweepArtifactCache: %v", err)
	}

	_, err = os.Stat(next)
	if !os.IsNotExist(err) {
		t.Error("the sweep stopped after an eviction that freed nothing, leaving the cache over its cap")
	}
}
