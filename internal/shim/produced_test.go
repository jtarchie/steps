package shim

// The cache is written on the PRODUCE path too (steps#138): what a fetch
// packs is filed under the digest the next step's offer will name.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// fetchAs is a fetch-all under a name, the shape a get's tree comes home in.
func (p *peer) fetchAs(artifact, dst string) {
	p.t.Helper()

	p.fetchWith(wire.Fetch{Artifact: artifact}, dst)
}

// TestFetchFilesWhatItPacked: a declared output fetched home is held by the
// worker afterwards, so offering it back — as the next step's input — costs
// nothing.
func TestFetchFilesWhatItPacked(t *testing.T) {
	root := t.TempDir()

	producer := newPeer(t, Options{Build: "test", Root: root})
	producer.hello()
	producer.exec("mkdir -p out/deep && echo made > out/deep/made.txt", nil)

	home := t.TempDir()
	producer.fetch([]string{"out"}, home)
	producer.goodbye()

	consumer := newPeer(t, Options{Build: "test", Root: root})
	consumer.hello()

	if answer := consumer.offerAnswer(home, "out"); answer != wire.FrameEnd {
		t.Errorf("the offer of what the worker itself produced was answered %v, want FrameEnd — the worker did not keep what it packed", answer)
	}
}

// TestFetchAllFilesTheTreeUnderItsName: a get's tree is the whole work
// directory, and it is filed under the name the next step will offer it by,
// not under the directory's own.
func TestFetchAllFilesTheTreeUnderItsName(t *testing.T) {
	root := t.TempDir()

	producer := newPeer(t, Options{Build: "test", Root: root})
	producer.hello()
	producer.exec("echo one > top.txt && mkdir nested && echo two > nested/deep.txt", nil)

	home := t.TempDir()
	producer.fetchAs("src", home)
	producer.goodbye()

	if got := mustRead(t, filepath.Join(home, "src", "nested", "deep.txt")); got != "two\n" {
		t.Fatalf("the tree came home as %q, want it under its artifact name", got)
	}

	consumer := newPeer(t, Options{Build: "test", Root: root})
	consumer.hello()

	if answer := consumer.offerAnswer(home, "src"); answer != wire.FrameEnd {
		t.Errorf("the offer of the fetched tree was answered %v, want FrameEnd", answer)
	}
}

// TestAVolatileWorkdirFilesNothingItProduced: on tmpfs the cache is memory,
// and a produced tree is optional — received ones still land, because the
// step needs them.
func TestAVolatileWorkdirFilesNothingItProduced(t *testing.T) {
	previous := fsInfo
	fsInfo = func(string) (string, uint64) { return "tmpfs", 1 << 30 }

	t.Cleanup(func() { fsInfo = previous })

	root := t.TempDir()

	producer := newPeer(t, Options{Build: "test", Root: root})
	producer.hello()
	producer.exec("mkdir out && echo made > out/made.txt", nil)

	home := t.TempDir()
	producer.fetch([]string{"out"}, home)
	producer.goodbye()

	entries, err := os.ReadDir(filepath.Join(root, "steps-shim", artifactCacheName))
	if err == nil && len(entries) > 0 {
		t.Errorf("the cache holds %d entries after a fetch on tmpfs, want none: %v", len(entries), entries)
	}

	consumer := newPeer(t, Options{Build: "test", Root: root})
	consumer.hello()

	if answer := consumer.offerAnswer(home, "out"); answer != wire.FrameNeed {
		t.Errorf("the offer was answered %v, want FrameNeed — a produced tree was filed in memory", answer)
	}
}

// TestDeferredFetchKeepsTheOutputsAndNamesThem: a deferred fetch sends no
// bytes — it files the outputs and answers with their digests, and a FrameGet
// for one of them is answered with the tree exactly as an offer would digest
// it.
func TestDeferredFetchKeepsTheOutputsAndNamesThem(t *testing.T) {
	root := t.TempDir()

	producer := newPeer(t, Options{Build: "test", Root: root})
	producer.hello()
	producer.exec("mkdir -p out/deep && echo made > out/deep/made.txt", nil)

	op := producer.next()
	producer.send(wire.FrameFetch, op, wire.Fetch{Paths: []string{"out"}, Defer: true})

	frame := producer.read()
	if frame.Type != wire.FrameEnd || frame.Op != op {
		t.Fatalf("a deferred fetch answered a type %d frame for op %d, want the End with what was kept", frame.Type, frame.Op)
	}

	var done wire.FetchDone

	err := wire.DecodeJSON(frame, &done)
	if err != nil {
		t.Fatalf("decoding what was kept: %v", err)
	}

	digest, ok := done.Artifacts["out"]
	if !ok || digest == "" {
		t.Fatalf("kept = %v, want out under its digest", done.Artifacts)
	}

	home := t.TempDir()
	producer.get("out", digest, home)
	producer.goodbye()

	if got := mustRead(t, filepath.Join(home, "out", "deep", "made.txt")); got != "made\n" {
		t.Errorf("the pulled tree holds %q, want what the command wrote", got)
	}

	if got := artifactDigest(t, home, "out"); got != digest {
		t.Errorf("the pulled tree digests as %s, want the %s it was kept under", got, digest)
	}
}

// TestGetRefusesWhatItDoesNotHold: a FrameGet for a digest this worker never
// filed is an error naming it, never an empty tree.
func TestGetRefusesWhatItDoesNotHold(t *testing.T) {
	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})
	peer.hello()

	op := peer.next()
	peer.send(wire.FrameGet, op, wire.Get{Name: "out", Digest: strings.Repeat("ab", 32)})

	frame := peer.readAny()
	if frame.Type != wire.FrameError {
		t.Fatalf("got a type %d frame, want a refusal", frame.Type)
	}

	var refusal wire.Error

	err := wire.DecodeJSON(frame, &refusal)
	if err != nil {
		t.Fatalf("decoding the refusal: %v", err)
	}

	if !strings.Contains(refusal.Message, `"out"`) {
		t.Errorf("the refusal %q does not name the artifact", refusal.Message)
	}

	peer.goodbye()
}

// TestADeferredFetchOnTmpfsKeepsNothing: with nothing filed, the answer names
// nothing, and the orchestrator fetches the ordinary way.
func TestADeferredFetchOnTmpfsKeepsNothing(t *testing.T) {
	previous := fsInfo
	fsInfo = func(string) (string, uint64) { return "tmpfs", 1 << 30 }

	t.Cleanup(func() { fsInfo = previous })

	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})
	peer.hello()
	peer.exec("mkdir out && echo made > out/made.txt", nil)

	op := peer.next()
	peer.send(wire.FrameFetch, op, wire.Fetch{Paths: []string{"out"}, Defer: true})

	frame := peer.read()
	if frame.Type != wire.FrameEnd {
		t.Fatalf("got a type %d frame, want the End", frame.Type)
	}

	var done wire.FetchDone

	_ = wire.DecodeJSON(frame, &done)

	if len(done.Artifacts) != 0 {
		t.Errorf("kept %v on tmpfs, want nothing", done.Artifacts)
	}

	peer.goodbye()
}

// get pulls one held tree into dst, as the orchestrator's Pull does.
func (p *peer) get(name, digest, dst string) {
	p.t.Helper()

	op := p.next()
	p.send(wire.FrameGet, op, wire.Get{Name: name, Digest: digest})

	reader, writer := io.Pipe()

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		err := wire.UnpackTree(reader, dst)
		_ = reader.CloseWithError(err)
	}()

	for {
		frame := p.read()
		if frame.Type == wire.FrameEnd {
			break
		}

		if frame.Type != wire.FrameData {
			p.t.Fatalf("unexpected type %d frame during a get", frame.Type)
		}

		_, err := writer.Write(frame.Payload)
		if err != nil {
			p.t.Fatalf("unpacking a get: %v", err)
		}
	}

	_ = writer.Close()

	wg.Wait()
}

// TestPushPutsAHeldTreeInTheStore: a FramePush uploads the held entry, packed
// exactly as a Get would stream it, to the URL it was handed.
func TestPushPutsAHeldTreeInTheStore(t *testing.T) {
	var (
		mu   sync.Mutex
		body []byte
	)

	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)

		mu.Lock()
		body = got
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(store.Close)

	root := t.TempDir()

	producer := newPeer(t, Options{Build: "test", Root: root})
	producer.hello()
	producer.exec("mkdir out && echo made > out/made.txt", nil)

	op := producer.next()
	producer.send(wire.FrameFetch, op, wire.Fetch{Paths: []string{"out"}, Defer: true})

	var done wire.FetchDone

	_ = wire.DecodeJSON(producer.read(), &done)

	op = producer.next()
	producer.send(wire.FramePush, op, wire.Push{Name: "out", Digest: done.Artifacts["out"], URL: store.URL + "/wire/" + done.Artifacts["out"]})

	if frame := producer.read(); frame.Type != wire.FrameEnd || frame.Op != op {
		t.Fatalf("push answered a type %d frame for op %d, want the End", frame.Type, frame.Op)
	}

	producer.goodbye()

	mu.Lock()
	defer mu.Unlock()

	if len(body) == 0 {
		t.Fatal("nothing reached the store")
	}

	// What arrived is the artifact under the digest it was pushed as.
	home := t.TempDir()

	err := unpackVerified(bytes.NewReader(body), home, done.Artifacts["out"], true)
	if err != nil {
		t.Fatalf("what the worker pushed does not verify against its digest: %v", err)
	}

	if got := mustRead(t, filepath.Join(home, "out", "made.txt")); got != "made\n" {
		t.Errorf("pushed tree holds %q", got)
	}
}

// TestPushRefusesWhatItDoesNotHold mirrors the Get: no tree, no upload, an
// error naming the artifact.
func TestPushRefusesWhatItDoesNotHold(t *testing.T) {
	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})
	peer.hello()

	op := peer.next()
	peer.send(wire.FramePush, op, wire.Push{Name: "out", Digest: strings.Repeat("ab", 32), URL: "http://127.0.0.1:1/never"})

	frame := peer.readAny()
	if frame.Type != wire.FrameError {
		t.Fatalf("got a type %d frame, want a refusal", frame.Type)
	}

	var refusal wire.Error

	_ = wire.DecodeJSON(frame, &refusal)

	if !strings.Contains(refusal.Message, `"out"`) {
		t.Errorf("the refusal %q does not name the artifact", refusal.Message)
	}

	peer.goodbye()
}

// TestGetUnderAnotherNameDoesNotEvict: an entry asked for under a name it was
// not filed as is refused, and the entry survives for the name that is right.
func TestGetUnderAnotherNameDoesNotEvict(t *testing.T) {
	root := t.TempDir()

	producer := newPeer(t, Options{Build: "test", Root: root})
	producer.hello()
	producer.exec("mkdir out && echo made > out/made.txt", nil)

	op := producer.next()
	producer.send(wire.FrameFetch, op, wire.Fetch{Paths: []string{"out"}, Defer: true})

	var done wire.FetchDone

	_ = wire.DecodeJSON(producer.read(), &done)

	op = producer.next()
	producer.send(wire.FrameGet, op, wire.Get{Name: "other", Digest: done.Artifacts["out"]})

	if frame := producer.readAny(); frame.Type != wire.FrameError {
		t.Fatalf("a get under the wrong name answered a type %d frame, want a refusal", frame.Type)
	}

	home := t.TempDir()
	producer.get("out", done.Artifacts["out"], home)

	if got := mustRead(t, filepath.Join(home, "out", "made.txt")); got != "made\n" {
		t.Errorf("after a wrong-name get the right one reads %q — the entry was evicted", got)
	}

	producer.goodbye()
}
