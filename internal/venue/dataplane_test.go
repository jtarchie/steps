package venue

// The URL data plane, end to end over a real shim: control frames on the
// tunnel, tree bytes through the store.

import (
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// countingS3 is the store's remote half, counting operations per key class so
// a test can say WHO moved bytes: tree blobs are content-keyed under wire/,
// fetch objects under wire/out-.
type countingS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	treePuts int
	treeGets int
	outPuts  int
	outGets  int
	// treeBytesOut is what the WORKER actually pulled down for step trees,
	// which is the number the reuse work exists to reduce. Counts, not bytes,
	// are the wrong measure there: a step that re-fetches a 64MB input it
	// already has is one GET and a real cost.
	treeBytesOut int
}

func newCountingS3(t *testing.T) (*countingS3, string) {
	t.Helper()

	fake := &countingS3{objects: map[string][]byte{}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	// A local: worker takes no root, so its shim caches artifacts under the
	// system temp dir — which is shared, and would let one test's cache
	// answer another test's fetch. Per-test here, and inherited by the shim
	// this process execs.
	t.Setenv("TMPDIR", t.TempDir())

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	return fake, "s3://cas/venue?endpoint=" + server.URL + "&region=us-east-1"
}

func (f *countingS3) count(path, method string) {
	out := strings.Contains(path, "/wire/out-")

	switch {
	case method == http.MethodPut && out:
		f.outPuts++
	case method == http.MethodPut:
		f.treePuts++
	case method == http.MethodGet && out:
		f.outGets++
	case method == http.MethodGet:
		f.treeGets++
	}
}

func (f *countingS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		f.objects[r.URL.Path] = body
		f.count(r.URL.Path, r.Method)

		w.WriteHeader(http.StatusOK)
	case http.MethodHead:
		if _, ok := f.objects[r.URL.Path]; !ok {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		body, ok := f.objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>NoSuchKey</Code></Error>`))

			return
		}

		f.count(r.URL.Path, r.Method)

		if !strings.Contains(r.URL.Path, "/wire/out-") {
			f.treeBytesOut += len(body)
		}

		_, _ = w.Write(body)
	case http.MethodDelete:
		delete(f.objects, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// TestVenueMovesTreesThroughTheStore is the data plane's whole promise: the
// step round-trips exactly as it does over the tunnel, while the bytes
// demonstrably travel via the store — the tree once up (orchestrator) and
// once down (worker), the outputs once up (worker) and once down
// (orchestrator).
func TestVenueMovesTreesThroughTheStore(t *testing.T) {
	fake, storeURL := newCountingS3(t)

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")
	mustMkdir(t, filepath.Join(cwd, "out"))

	spec := localWorker(t, cwd, "out")
	spec.ArtifactStore = storeURL

	runner := newLocalRunner(t, spec)

	err := runner.Run(context.Background(), "cat data/seed.txt > out/report.txt")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := mustRead(t, filepath.Join(cwd, "out", "report.txt"))
	if got != "seed\n" {
		t.Errorf("out/report.txt = %q, want %q", got, "seed\n")
	}

	// One per ARTIFACT now, not one per tree: this step's cwd holds data/ and
	// out/, and naming them separately is what lets a later step skip the one
	// it already has.
	if fake.treePuts != 2 || fake.treeGets != 2 {
		t.Errorf("tree blobs: %d puts, %d gets; want 2 and 2 — one per artifact, up once and down once",
			fake.treePuts, fake.treeGets)
	}

	if fake.outPuts != 1 || fake.outGets != 1 {
		t.Errorf("output blobs: %d puts, %d gets; want 1 and 1 — the outputs did not travel through the store exactly once",
			fake.outPuts, fake.outGets)
	}
}

// TestVenueFallsBackToTheTunnelWithALegacyShim pins the floor: a shim that
// never learned the plane takes its trees over the tunnel, and the store —
// though configured — is never touched. The tunnel is always sufficient.
func TestVenueFallsBackToTheTunnelWithALegacyShim(t *testing.T) {
	fake, storeURL := newCountingS3(t)
	t.Setenv(legacyShimEnv, "1")

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")
	mustMkdir(t, filepath.Join(cwd, "out"))

	spec := localWorker(t, cwd, "out")
	spec.ArtifactStore = storeURL

	runner := newLocalRunner(t, spec)

	err := runner.Run(context.Background(), "cat data/seed.txt > out/report.txt")
	if err != nil {
		t.Fatalf("Run against a legacy shim: %v", err)
	}

	if got := mustRead(t, filepath.Join(cwd, "out", "report.txt")); got != "seed\n" {
		t.Errorf("out/report.txt = %q, want %q", got, "seed\n")
	}

	if len(fake.objects) != 0 {
		t.Errorf("the store holds %d objects after a tunnel-only session, want none", len(fake.objects))
	}
}

// TestVenueRedialReusesTheUploadedTree is what content-keying the tree buys:
// a worker that dies is redialed, and the replacement session finds the tree
// already in the store — the property a spot eviction's replacement venue
// (#82) resumes on.
func TestVenueRedialReusesTheUploadedTree(t *testing.T) {
	fake, storeURL := newCountingS3(t)

	// No outputs: nothing is fetched back, so the tree — and every artifact
	// key in it — is identical across the redial.
	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")

	spec := localWorker(t, cwd)
	spec.ArtifactStore = storeURL

	runner := newLocalRunner(t, spec)

	err := runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("the first command failed: %v", err)
	}

	// Not asserted on: the shim is killed while answering, so under load its
	// exit frame can win the race and the kill reads as success. The redial
	// below is what this test is about, and it happens either way.
	_ = killTheShim(t, runner)

	err = runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("the command after the redial failed: %v", err)
	}

	if fake.treePuts != 1 {
		t.Errorf("tree puts = %d, want 1 — the redial re-uploaded an artifact the store already held", fake.treePuts)
	}

	// One, not two. The redial opens a NEW session with a new work directory,
	// and the artifact it needs is one this worker already pulled down for
	// the session that died — which is the same reuse two steps of a job get,
	// arriving here for free.
	if fake.treeGets != 1 {
		t.Errorf("tree gets = %d, want 1 — the redial re-fetched an artifact the worker already had", fake.treeGets)
	}
}

// TestWorkerDoesNotRefetchAnInputItAlreadyHas is phase 3's whole point,
// stated as the number it moves.
//
// Two placed steps of one job share an input and declare different outputs.
// Their TREES therefore differ — each carries its own output directory — so a
// whole-tree content key misses on the second step and the worker pulls the
// shared payload down twice. Measured on a real job: a 64MB input through
// three steps moved 192MB to the worker, with and without a store.
//
// The bytes are in the shared input, not in the tree, so this counts bytes
// rather than requests: one re-fetch is one GET and a whole payload.
func TestWorkerDoesNotRefetchAnInputItAlreadyHas(t *testing.T) {
	fake, storeURL := newCountingS3(t)

	// Incompressible: trees cross zstd-compressed, and a megabyte of one
	// repeated byte measures nothing at all. The first draft of this test
	// used exactly that and passed without the feature existing.
	payload := make([]byte, 1<<20)
	_, err := rand.Read(payload)
	if err != nil {
		t.Fatalf("making an incompressible payload: %v", err)
	}

	run := func(outputs string) {
		t.Helper()

		cwd := t.TempDir()
		mustWrite(t, filepath.Join(cwd, "data", "big.txt"), string(payload))
		mustMkdir(t, filepath.Join(cwd, outputs))

		spec := localWorker(t, cwd, outputs)
		spec.ArtifactStore = storeURL

		runner := newLocalRunner(t, spec)

		err := runner.Run(context.Background(), "wc -c < data/big.txt > "+outputs+"/n")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	run("out1")
	run("out2")

	// One payload, not two. The slack covers tar and frame overhead on the
	// second step's own (empty) output directory.
	if fake.treeBytesOut > 3*len(payload)/2 {
		t.Errorf("the worker pulled %d bytes of tree for two steps sharing one %d-byte input — it re-fetched what it already had",
			fake.treeBytesOut, len(payload))
	}
}

// TestTunnelDoesNotResendAnInputTheWorkerHas is the store test's twin on the
// other plane.
//
// The tunnel is the slower of the two — it is what an SSM session carries at
// single-digit MB/s — so re-sending a shared input costs more here, not less.
// Counted by what the shim actually holds: two steps sharing one input leave
// ONE copy of it in the worker's cache, not two.
func TestTunnelDoesNotResendAnInputTheWorkerHas(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)

	payload := make([]byte, 1<<20)

	_, err := rand.Read(payload)
	if err != nil {
		t.Fatalf("making an incompressible payload: %v", err)
	}

	sent := make([]int64, 0, 2)

	for _, outputs := range []string{"out1", "out2"} {
		cwd := t.TempDir()
		mustWrite(t, filepath.Join(cwd, "data", "big.txt"), string(payload))
		mustMkdir(t, filepath.Join(cwd, outputs))

		placed := newLocalRunner(t, localWorker(t, cwd, outputs))

		runErr := placed.Run(context.Background(), "wc -c < data/big.txt > "+outputs+"/n")
		if runErr != nil {
			t.Fatalf("Run: %v", runErr)
		}

		remote, ok := placed.(runner)
		if !ok {
			t.Fatalf("runner is %T, not a placed one", placed)
		}

		sent = append(sent, remote.session.sentArtifactBytes.Load())
	}

	if sent[0] < int64(len(payload))/2 {
		t.Fatalf("the first step sent %d bytes, want the whole input — the test is measuring nothing", sent[0])
	}

	// The second step's tree shares that input and differs only by an empty
	// output directory, so what crosses is that directory and nothing else.
	if sent[1] > int64(len(payload))/2 {
		t.Errorf("the second step sent %d bytes for an input the worker already had, want almost none", sent[1])
	}
}
