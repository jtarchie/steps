package shim

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/compress"
	"github.com/jtarchie/steps/internal/wire"
)

// blobHost hands out one blob and collects one, standing in for the store the
// orchestrator presigned. The shim must treat the URL as the entire
// authority, so plain handlers are the honest fake.
type blobHost struct {
	mu       sync.Mutex
	tree     []byte
	received []byte
}

func (b *blobHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		_, _ = w.Write(b.tree)
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		b.received = body

		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// packTree is a tree as it lives in the store: zstd over the tar codec.
// packTree packs named artifacts the way the store plane now sends them:
// one blob per top-level entry, not one for the whole tree.
func packTree(t *testing.T, src string, names ...string) []byte {
	t.Helper()

	var buf bytes.Buffer

	err := compress.Pack(&buf, true, func(w io.Writer) error {
		return wire.PackPaths(w, src, names)
	})
	if err != nil {
		t.Fatalf("packing the tree: %v", err)
	}

	return buf.Bytes()
}

// TestShimSpeaksTheURLPlane is the negotiated plane end to end on the worker
// side: the tree arrives by GET, the command runs against it, and the outputs
// leave by PUT — with no data frames in either direction.
func TestShimSpeaksTheURLPlane(t *testing.T) {
	t.Parallel()

	host := &blobHost{}
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)

	src := t.TempDir()

	err := os.WriteFile(filepath.Join(src, "seed.txt"), []byte("seed\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	host.tree = packTree(t, src, "seed.txt")

	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})
	peer.helloWithPlane()

	// Upload: one control frame carrying the URL, then the ack.
	peer.ackedSend(wire.FrameUpload, wire.Upload{Artifacts: []wire.UploadArtifact{
		{Name: "seed.txt", Digest: artifactDigest(t, src, "seed.txt"), URL: server.URL + "/wire/tree"},
	}})

	stdout, _, exit := peer.exec("cat seed.txt; mkdir -p out; cp seed.txt out/copy.txt", nil)
	if !exit.Started || exit.Code != 0 || stdout != "seed\n" {
		t.Fatalf("exit = %+v, stdout = %q; want a clean run against the fetched tree", exit, stdout)
	}

	// Fetch: one control frame carrying the PUT URL, the ack, and the bytes
	// on the host rather than on the wire.
	peer.ackedSend(wire.FrameFetch, wire.Fetch{Paths: []string{"out"}, URL: server.URL + "/wire/out-1"})

	if len(host.received) == 0 {
		t.Fatal("the shim shipped no outputs to the URL")
	}

	dst := t.TempDir()

	err = compress.Unpack(bytes.NewReader(host.received), true, func(r io.Reader) error {
		return wire.UnpackTree(r, dst)
	})
	if err != nil {
		t.Fatalf("what the shim PUT is not a store blob: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "out", "copy.txt")) //nolint:gosec // a path this test just built
	if err != nil || string(got) != "seed\n" {
		t.Fatalf("shipped output = %q, %v; want the command's output", got, err)
	}
}

// helloWithPlane opens the session on the URL plane, failing the test if the
// shim declines it.
func (p *peer) helloWithPlane() {
	p.t.Helper()

	op := p.next()
	p.send(wire.FrameHello, op, wire.Hello{
		Protocol: wire.Protocol, Build: "test", Session: "url-plane-under-test",
		Compression: wire.CompressionZstd, DataPlane: wire.DataPlaneURLs,
	})

	var ok wire.HelloOK

	err := wire.DecodeJSON(p.read(), &ok)
	if err != nil {
		p.t.Fatalf("decoding the hello answer: %v", err)
	}

	if ok.DataPlane != wire.DataPlaneURLs {
		p.t.Fatalf("DataPlane = %q, want %q — the shim did not accept the plane", ok.DataPlane, wire.DataPlaneURLs)
	}
}

// ackedSend sends one control frame and requires its FrameEnd acknowledgement.
func (p *peer) ackedSend(frameType wire.FrameType, payload any) {
	p.t.Helper()

	op := p.next()
	p.send(frameType, op, payload)

	if frame := p.read(); frame.Type != wire.FrameEnd || frame.Op != op {
		p.t.Fatalf("expected a type %d frame to be acknowledged, got a type %d frame for operation %d", frameType, frame.Type, frame.Op)
	}
}

// TestShimReportsAStoreItCannotReach pins the failure shape: a worker whose
// egress cannot reach the store answers with an error frame naming the
// status or the dial failure — an operation error over a live session, never
// a wedge waiting for data frames that are not coming.
func TestShimReportsAStoreItCannotReach(t *testing.T) {
	t.Parallel()

	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})

	op := peer.next()
	peer.send(wire.FrameHello, op, wire.Hello{
		Protocol: wire.Protocol, Build: "test", Session: "no-egress-under-test",
		DataPlane: wire.DataPlaneURLs,
	})
	peer.read()

	op = peer.next()
	// A port nothing listens on: the shape of blocked egress.
	peer.send(wire.FrameUpload, op, wire.Upload{Artifacts: []wire.UploadArtifact{
		{Name: "data", Digest: "unreachable", URL: "http://127.0.0.1:1/wire/tree"},
	}})

	frame, err := peer.decoder.Read()
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}

	if frame.Type != wire.FrameError {
		t.Fatalf("frame type = %d, want an error frame", frame.Type)
	}
}
