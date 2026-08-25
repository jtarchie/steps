package venue

// The URL data plane, end to end over a real shim: control frames on the
// tunnel, tree bytes through the store.

import (
	"context"
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
}

func newCountingS3(t *testing.T) (*countingS3, string) {
	t.Helper()

	fake := &countingS3{objects: map[string][]byte{}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

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

	if fake.treePuts != 1 || fake.treeGets != 1 {
		t.Errorf("tree blobs: %d puts, %d gets; want 1 and 1 — the tree did not travel through the store exactly once",
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

	// No outputs: nothing is fetched back, so the local tree — and its
	// content key — is identical across the redial.
	spec := localWorker(t, t.TempDir())
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
		t.Errorf("tree puts = %d, want 1 — the redial re-uploaded a tree the store already held", fake.treePuts)
	}

	if fake.treeGets != 2 {
		t.Errorf("tree gets = %d, want 2 — each session's shim fetches the tree once", fake.treeGets)
	}
}
