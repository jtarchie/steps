package shim

// Compression is negotiated at the hello and applies to the tree transfers —
// never to control frames, whose size is noise beside a tree's.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/compress"
	"github.com/jtarchie/steps/internal/wire"
)

// helloWith opens the session proposing a compression.
func (p *peer) helloWith(compression string) wire.HelloOK {
	p.t.Helper()

	op := p.next()
	p.send(wire.FrameHello, op, wire.Hello{
		Protocol: wire.Protocol, Build: "test", Session: "compression-under-test",
		Compression: compression,
	})

	var ok wire.HelloOK

	err := wire.DecodeJSON(p.read(), &ok)
	if err != nil {
		p.t.Fatalf("decoding the hello answer: %v", err)
	}

	return ok
}

// uploadZstd sends a tree the way a compressing venue will: one zstd stream
// carrying the tar, chunked into data frames.
func (p *peer) uploadZstd(src string) {
	p.t.Helper()

	var buf bytes.Buffer

	encoder, err := compress.NewWriter(&buf)
	if err != nil {
		p.t.Fatalf("opening the zstd stream: %v", err)
	}

	err = wire.PackTree(encoder, src)
	if err != nil {
		p.t.Fatalf("packing %q: %v", src, err)
	}

	err = encoder.Close()
	if err != nil {
		p.t.Fatalf("closing the zstd stream: %v", err)
	}

	op := p.next()
	p.sendEmpty(wire.FrameUpload, op)

	err = p.encoder.Write(wire.Frame{Type: wire.FrameData, Op: op, Payload: buf.Bytes()})
	if err != nil {
		p.t.Fatalf("sending the compressed tree: %v", err)
	}

	p.sendEmpty(wire.FrameEnd, op)

	if frame := p.read(); frame.Type != wire.FrameEnd || frame.Op != op {
		p.t.Fatalf("expected the upload to be acknowledged, got a type %d frame for operation %d", frame.Type, frame.Op)
	}
}

// TestShimSpeaksZstdWhenOffered is the negotiated path in both directions:
// the shim echoes the token, decodes a compressed upload, and answers a fetch
// with bytes that are zstd rather than tar.
func TestShimSpeaksZstdWhenOffered(t *testing.T) {
	t.Parallel()

	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})

	ok := peer.helloWith(wire.CompressionZstd)
	if ok.Compression != wire.CompressionZstd {
		t.Fatalf("Compression = %q, want %q — the shim did not accept what was offered", ok.Compression, wire.CompressionZstd)
	}

	src := t.TempDir()

	err := os.WriteFile(filepath.Join(src, "seed.txt"), []byte("seed\n"), 0o600)
	if err != nil {
		t.Fatalf("writing the seed: %v", err)
	}

	peer.uploadZstd(src)

	stdout, _, exit := peer.exec("cat seed.txt; mkdir -p out; cp seed.txt out/copy.txt", nil)
	if !exit.Started || exit.Code != 0 {
		t.Fatalf("exit = %+v, want a clean run against the decompressed tree", exit)
	}

	if stdout != "seed\n" {
		t.Errorf("stdout = %q, want the uploaded content", stdout)
	}

	dst := peer.fetchZstd([]string{"out"})

	got, err := os.ReadFile(filepath.Join(dst, "out", "copy.txt")) //nolint:gosec // a path this test just built
	if err != nil || string(got) != "seed\n" {
		t.Errorf("out/copy.txt = %q, %v; want the output the command produced", got, err)
	}
}

// fetchZstd asks for named subtrees, requires the answer to be one zstd
// stream, and unpacks it into a fresh directory.
func (p *peer) fetchZstd(paths []string) string {
	p.t.Helper()

	op := p.next()
	p.send(wire.FrameFetch, op, wire.Fetch{Paths: paths})

	var compressed bytes.Buffer

	for {
		frame := p.read()
		if frame.Type == wire.FrameEnd {
			break
		}

		if frame.Type != wire.FrameData {
			p.t.Fatalf("unexpected type %d frame during a fetch", frame.Type)
		}

		compressed.Write(frame.Payload)
	}

	reader, err := compress.NewReader(&compressed)
	if err != nil {
		p.t.Fatalf("the fetch did not come back as zstd: %v", err)
	}

	defer func() { _ = reader.Close() }()

	dst := p.t.TempDir()

	err = wire.UnpackTree(reader, dst)
	if err != nil {
		p.t.Fatalf("the decompressed fetch is not the tar codec's stream: %v", err)
	}

	return dst
}

// TestShimIgnoresACompressionItDoesNotKnow pins the degradation story: an
// unknown token is an orchestrator newer than this shim, and the answer is
// raw — the floor both ends always share — never a refusal.
func TestShimIgnoresACompressionItDoesNotKnow(t *testing.T) {
	t.Parallel()

	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})

	ok := peer.helloWith("brotli")
	if ok.Compression != "" {
		t.Fatalf("Compression = %q, want empty — a shim must not claim a token it cannot decode", ok.Compression)
	}

	src := t.TempDir()

	err := os.WriteFile(filepath.Join(src, "seed.txt"), []byte("raw\n"), 0o600)
	if err != nil {
		t.Fatalf("writing the seed: %v", err)
	}

	// Raw upload, exactly as an old orchestrator sends one.
	peer.upload(src)

	stdout, _, exit := peer.exec("cat seed.txt", nil)
	if !exit.Started || exit.Code != 0 || stdout != "raw\n" {
		t.Fatalf("exit = %+v, stdout = %q; want the raw path untouched", exit, stdout)
	}
}

// TestShimRefusesRawBytesAfterNegotiatingZstd proves the negotiated path
// really decodes: a shim that quietly passed raw tar through after agreeing
// to zstd would make TestShimSpeaksZstdWhenOffered vacuous, and would let a
// venue that forgot to compress ship trees that only sometimes unpack.
func TestShimRefusesRawBytesAfterNegotiatingZstd(t *testing.T) {
	t.Parallel()

	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})
	peer.helloWith(wire.CompressionZstd)

	src := t.TempDir()

	err := os.WriteFile(filepath.Join(src, "seed.txt"), []byte("seed\n"), 0o600)
	if err != nil {
		t.Fatalf("writing the seed: %v", err)
	}

	op := peer.next()
	peer.sendEmpty(wire.FrameUpload, op)

	writer := peer.dataWriter(op)

	err = wire.PackTree(writer, src)
	if err != nil {
		t.Fatalf("packing %q: %v", src, err)
	}

	err = writer.flush()
	if err != nil {
		t.Fatalf("flushing the upload: %v", err)
	}

	peer.sendEmpty(wire.FrameEnd, op)

	frame, err := peer.decoder.Read()
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}

	if frame.Type != wire.FrameError {
		t.Fatalf("frame type = %d, want an error frame — raw tar after negotiating zstd was accepted", frame.Type)
	}
}
