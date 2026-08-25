package blobstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeS3 answers the three verbs the store speaks, against a map. Auth is not
// checked: what these tests pin is the byte flow and the key layout.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newTestStore(t *testing.T) (*Store, *fakeS3) {
	t.Helper()

	fake := &fakeS3{objects: map[string][]byte{}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	opts, err := Parse("s3://cas/team?endpoint=" + server.URL + "&region=us-east-1")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	store, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return store, fake
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
			// GetObject reads the error code from the XML body.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>NoSuchKey</Code></Error>`))

			return
		}

		_, _ = w.Write(body)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// TestBlobRoundTrip is the store's whole contract: a tree goes up under a
// digest, its existence is answerable, and it comes back byte-for-byte.
func TestBlobRoundTrip(t *testing.T) {
	store, fake := newTestStore(t)
	ctx := context.Background()

	src := t.TempDir()

	err := os.WriteFile(filepath.Join(src, "result.txt"), []byte("payload\n"), 0o600)
	if err != nil {
		t.Fatalf("writing the tree: %v", err)
	}

	const digest = "abc123"

	if hasTree(t, store, digest) {
		t.Fatal("HasTree before upload = true, want false")
	}

	err = store.PutTree(ctx, digest, src)
	if err != nil {
		t.Fatalf("PutTree: %v", err)
	}

	assertStoredAt(t, fake, "/cas/team/blobs/"+digest)

	if !hasTree(t, store, digest) {
		t.Fatal("HasTree after upload = false, want true")
	}

	dst := filepath.Join(t.TempDir(), "restored")

	err = store.GetTree(ctx, digest, dst)
	if err != nil {
		t.Fatalf("GetTree: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "result.txt")) //nolint:gosec // a path this test just built
	if err != nil || string(got) != "payload\n" {
		t.Fatalf("restored tree = %q, %v; want the uploaded content", got, err)
	}
}

func hasTree(t *testing.T, store *Store, digest string) bool {
	t.Helper()

	has, err := store.HasTree(context.Background(), digest)
	if err != nil {
		t.Fatalf("HasTree: %v", err)
	}

	return has
}

// assertStoredAt pins the key layout, which is part of the contract: a
// lifecycle rule and a human both need to know where blobs live.
func assertStoredAt(t *testing.T, fake *fakeS3, path string) {
	t.Helper()

	if _, ok := fake.objects[path]; !ok {
		keys := make([]string, 0, len(fake.objects))
		for key := range fake.objects {
			keys = append(keys, key)
		}

		t.Fatalf("blob not at %s; stored keys: %v", path, keys)
	}
}

// TestGetTreeReportsAMissingBlobAsSuch pins the error a caller treats as an
// ordinary miss, distinct from a store that is broken.
func TestGetTreeReportsAMissingBlobAsSuch(t *testing.T) {
	store, _ := newTestStore(t)

	err := store.GetTree(context.Background(), "nope", filepath.Join(t.TempDir(), "restored"))
	if !errors.Is(err, ErrMissingBlob) {
		t.Fatalf("GetTree of an absent digest = %v, want ErrMissingBlob", err)
	}
}

// TestParseRefusals pins that a mapping that can never work is refused when
// read, not when first used.
func TestParseRefusals(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "gs://bucket/prefix", "s3://"} {
		_, err := Parse(raw)
		if !errors.Is(err, ErrStore) {
			t.Errorf("Parse(%q) = %v, want ErrStore", raw, err)
		}
	}

	opts, err := Parse("s3://bucket/a/b/")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if opts.Bucket != "bucket" || opts.Prefix != "a/b" {
		t.Errorf("Parse = %+v, want bucket %q prefix %q", opts, "bucket", "a/b")
	}
}
