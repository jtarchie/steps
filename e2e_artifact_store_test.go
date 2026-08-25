package main

// The fleet-wide artifact cache: step outputs mirrored to a content-addressed
// store, so bytes evicted locally — or never present on this machine — are
// materialized back instead of re-earned.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeS3 is the store's remote half: presigned-style GET/PUT/HEAD over plain
// HTTP against an in-memory map, which is all S3 is to this feature. Auth is
// deliberately not checked — what the tests pin is the byte flow.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
	gets    int
}

func newFakeS3(t *testing.T) (*fakeS3, string) {
	t.Helper()

	fake := &fakeS3{objects: map[string][]byte{}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	// The SDK's credential chain runs before any request; static nonsense
	// keeps it satisfied and hermetic.
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	return fake, server.URL
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case http.MethodPut:
		body := make([]byte, 0, 1024)
		buf := make([]byte, 32*1024)

		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)

			if err != nil {
				break
			}
		}

		f.objects[r.URL.Path] = body
		f.puts++

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

		f.gets++

		_, _ = w.Write(body)
	case http.MethodDelete:
		delete(f.objects, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// wireObjects counts the objects the venue data plane stored, as opposed to
// the step cache's blobs/ mirror.
func (f *fakeS3) wireObjects() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	count := 0

	for key := range f.objects {
		if strings.Contains(key, "/wire/") {
			count++
		}
	}

	return count
}

// artifactStorePipeline is a two-step job whose first step records every real
// execution in a side-effect file — the thing a cache hit must NOT touch —
// and whose second step consumes the first's output.
func artifactStorePipeline(t *testing.T, dir, root, publishLine string) string {
	t.Helper()

	return writePipeline(t, dir, `
workspace:
  root: `+root+`
jobs:
- name: build
  plan:
  - task: expensive
    outputs: [data]
    run: |
      echo ran >> `+filepath.Join(dir, "executions")+`
      echo payload > data/result.txt
  - task: publish
    inputs: [data]
    run: `+publishLine+`
`)
}

// TestEndToEndArtifactStoreRematerializesEvictedOutputs is #80's fleet-cache
// promise, end to end: a step whose cached bytes are gone locally — evicted,
// or never on this machine — is skipped by materializing them from the
// content-addressed store, not re-run. The state database is the truth that
// says skipping is sound; S3 only holds the bytes.
func TestEndToEndArtifactStoreRematerializesEvictedOutputs(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "ws-root")
	fake, url := newFakeS3(t)

	storeFlag := "s3://cas/team?endpoint=" + url + "&region=us-east-1"

	path := artifactStorePipeline(t, dir, root,
		"cp data/result.txt "+filepath.Join(dir, "published.txt"))
	mustRun(t, path, "--artifact-store", storeFlag)

	if fake.puts == 0 {
		t.Fatal("the run published nothing to the artifact store")
	}

	// Evict the local bytes: the step cache entries are gone, the state
	// database survives — the shape of a second machine given the state, or
	// a cache swept locally.
	err := os.RemoveAll(filepath.Join(root, "steps-cache"))
	if err != nil {
		t.Fatalf("evicting the local step cache: %v", err)
	}

	// A changed SECOND step keeps the chain skip from answering for the whole
	// job, so the first step has to come from the step cache — whose bytes
	// are now only in the store.
	edited := artifactStorePipeline(t, dir, root,
		"cp data/result.txt "+filepath.Join(dir, "published-again.txt"))

	err = os.Rename(edited, path)
	if err != nil {
		t.Fatalf("editing the pipeline: %v", err)
	}

	mustRun(t, path, "--artifact-store", storeFlag)

	executions := readFileString(t, filepath.Join(dir, "executions"))
	if strings.Count(executions, "ran") != 1 {
		t.Fatalf("the expensive step ran %d times, want 1 — evicted outputs were re-earned instead of materialized from the store",
			strings.Count(executions, "ran"))
	}

	if fake.gets == 0 {
		t.Fatal("nothing was fetched from the artifact store, so where did the outputs come from?")
	}

	published := readFileString(t, filepath.Join(dir, "published-again.txt"))
	if published != "payload\n" {
		t.Errorf("published = %q, want the payload the cached step produced", published)
	}
}

// TestEndToEndArtifactStorePlacedStepUsesTheDataPlane is the venue half of
// the store, through the real CLI: a tagged step's trees move via the store
// while the tunnel carries control frames — and the step's outputs still land
// exactly where a local step's would.
func TestEndToEndArtifactStorePlacedStepUsesTheDataPlane(t *testing.T) {
	dir := t.TempDir()
	fake, url := newFakeS3(t)

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: remote
    tags: [box]
    outputs: [out]
    run: echo made-remotely > out/f.txt
  - task: publish
    inputs: [out]
    run: cp out/f.txt `+filepath.Join(dir, "published.txt")+`
`)

	mustRun(t, path,
		"--worker", "box=local:",
		"--artifact-store", "s3://cas/team?endpoint="+url+"&region=us-east-1")

	published := readFileString(t, filepath.Join(dir, "published.txt"))
	if !strings.Contains(published, "made-remotely") {
		t.Errorf("published = %q, want the placed step's output", published)
	}

	if fake.wireObjects() == 0 {
		t.Fatal("the placed step moved no trees through the store — the data plane was not used")
	}
}

// TestEndToEndArtifactStoreIsOptIn pins that the flag is the whole switch: a
// pipeline run without it never touches the network, whatever the workspace
// configuration says.
func TestEndToEndArtifactStoreIsOptIn(t *testing.T) {
	dir := t.TempDir()
	fake, _ := newFakeS3(t)

	path := artifactStorePipeline(t, dir, filepath.Join(dir, "ws-root"),
		"cp data/result.txt "+filepath.Join(dir, "published.txt"))
	mustRun(t, path)

	if fake.puts != 0 || fake.gets != 0 {
		t.Fatalf("a run without --artifact-store reached the store (%d puts, %d gets)", fake.puts, fake.gets)
	}
}
