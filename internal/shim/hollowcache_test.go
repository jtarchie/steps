package shim

// A cache entry a temp cleaner emptied (steps#119): cleaners age files one at a time and leave the directories, so the damaged entry still exists under its digest.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// hollowSource is an artifact with one file a cleaner will take and one it will leave, nested so a directory survives the deletion.
func hollowSource(t *testing.T) string {
	t.Helper()

	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "data", "kept.txt"), "kept\n")
	mustWrite(t, filepath.Join(src, "data", "deep", "gone.txt"), "gone\n")

	return src
}

// hollowOut deletes one file from a cached artifact and nothing else, which is what a temp cleaner leaves.
func hollowOut(t *testing.T, entry, name string) {
	t.Helper()

	err := os.Remove(filepath.Join(entry, name))
	if err != nil {
		t.Fatalf("hollowing %s: %v", name, err)
	}
}

// assertPlacedWhole fails unless both of hollowSource's files reached the work directory.
func assertPlacedWhole(t *testing.T, workdir string) {
	t.Helper()

	for name, want := range map[string]string{"data/kept.txt": "kept\n", "data/deep/gone.txt": "gone\n"} {
		got, err := os.ReadFile(filepath.Join(workdir, name)) //nolint:gosec // a path this test built
		if err != nil {
			t.Errorf("%s was not placed: %v — the worker served a hollowed cache entry as the whole artifact", name, err)

			continue
		}

		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// goodbye ends a peer's session; every peer shares hello's session name, so the next one would otherwise open this one's work directory.
func (p *peer) goodbye() {
	p.t.Helper()

	p.sendEmpty(wire.FrameBye, p.next())

	err := p.wait()
	if err != nil {
		p.t.Fatalf("Serve: %v", err)
	}
}

// offerAnswer offers one artifact and answers the shim's reply: FrameEnd for a hit, FrameNeed for a miss.
func (p *peer) offerAnswer(src, name string) wire.FrameType {
	p.t.Helper()

	p.send(wire.FrameUpload, p.next(), wire.Upload{
		Artifacts: []wire.UploadArtifact{{Name: name, Digest: artifactDigest(p.t, src, name)}},
	})

	return p.read().Type
}

// TestUploadRefetchesAnEntryATempCleanerHollowed is the tunnel plane: a hollowed entry is a miss the shim asks the bytes for, and the refetched tree REPLACES the entry, or every later step would refetch it forever.
func TestUploadRefetchesAnEntryATempCleanerHollowed(t *testing.T) {
	root := t.TempDir()
	src := hollowSource(t)

	first := newPeer(t, Options{Build: "test", Root: root})
	first.hello()
	first.upload(src)
	first.goodbye()

	hollowOut(t, filepath.Join(root, "steps-shim", artifactCacheName, artifactDigest(t, src, "data")), "data/deep/gone.txt")

	second := newPeer(t, Options{Build: "test", Root: root})
	ok := second.hello()
	second.upload(src)

	assertPlacedWhole(t, ok.Workdir)
	second.goodbye()

	third := newPeer(t, Options{Build: "test", Root: root})
	third.hello()

	if answer := third.offerAnswer(src, "data"); answer != wire.FrameEnd {
		t.Errorf("answer = type %d, want FrameEnd: the refetched tree did not replace the hollowed entry, so every later step pays the transfer again", answer)
	}
}

// TestUploadStillHitsAnArtifactThatIsAnEmptyDirectory guards the tempting shortcut: an entry with no files is a real artifact (an output declared and not yet written), not a hollowed one.
func TestUploadStillHitsAnArtifactThatIsAnEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	src := t.TempDir()
	mustMkdir(t, filepath.Join(src, "out"))

	first := newPeer(t, Options{Build: "test", Root: root})
	first.hello()
	first.upload(src)
	first.goodbye()

	second := newPeer(t, Options{Build: "test", Root: root})
	second.hello()

	if answer := second.offerAnswer(src, "out"); answer != wire.FrameEnd {
		t.Errorf("answer = type %d, want FrameEnd: an empty directory is the whole of that artifact, and refetching it every step is a cache that never hits", answer)
	}
}

// countingHost counts the GETs a blobHost answers, which on the store plane is how many times the shim fetched.
type countingHost struct {
	http.Handler

	gets atomic.Int32
}

func (c *countingHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		c.gets.Add(1)
	}

	c.Handler.ServeHTTP(w, r)
}

// storePlane serves hollowSource's artifact by GET and places it into fresh work directories over one cache.
type storePlane struct {
	t        *testing.T
	host     *countingHost
	cache    string
	artifact wire.UploadArtifact
}

func newStorePlane(t *testing.T) *storePlane {
	t.Helper()

	src := hollowSource(t)

	host := &countingHost{Handler: &blobHost{tree: packTree(t, src, "data")}}
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)

	return &storePlane{
		t:        t,
		host:     host,
		cache:    t.TempDir(),
		artifact: wire.UploadArtifact{Name: "data", Digest: artifactDigest(t, src, "data"), URL: server.URL + "/wire/tree"},
	}
}

func (p *storePlane) place() string {
	p.t.Helper()

	s := &session{workdir: p.t.TempDir()}

	err := s.placeArtifact(p.t.Context(), p.cache, p.artifact)
	if err != nil {
		p.t.Fatalf("placeArtifact: %v", err)
	}

	return s.workdir
}

func (p *storePlane) entry() string {
	return filepath.Join(p.cache, p.artifact.Digest)
}

// TestPlaceArtifactRefetchesAnEntryATempCleanerHollowed is the same on the store plane, where the fetch is a GET.
func TestPlaceArtifactRefetchesAnEntryATempCleanerHollowed(t *testing.T) {
	plane := newStorePlane(t)

	plane.place()
	hollowOut(t, plane.entry(), "data/deep/gone.txt")

	assertPlacedWhole(t, plane.place())

	if got := plane.host.gets.Load(); got != 2 {
		t.Errorf("fetched %d times after the entry was hollowed, want 2: a hollowed entry is a miss", got)
	}

	plane.place()

	if got := plane.host.gets.Load(); got != 2 {
		t.Errorf("fetched %d times, want still 2: the refetched tree did not replace the hollowed entry", got)
	}
}

// TestPlaceArtifactRefetchesAnEntryItCannotRead: an entry that cannot be re-digested cannot be proven whole, and that is a miss, not a step failed over the cache's own damage.
func TestPlaceArtifactRefetchesAnEntryItCannotRead(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a mode-0000 file, so there is no unreadable entry to make")
	}

	plane := newStorePlane(t)

	plane.place()

	err := os.Chmod(filepath.Join(plane.entry(), "data", "kept.txt"), 0)
	if err != nil {
		t.Fatal(err)
	}

	assertPlacedWhole(t, plane.place())

	if got := plane.host.gets.Load(); got != 2 {
		t.Errorf("fetched %d times after the entry became unreadable, want 2", got)
	}
}
