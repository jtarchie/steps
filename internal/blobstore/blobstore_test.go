package blobstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// TestRegionQueryOverridesTheAmbientRegion: ?region= is the knob for a bucket outside the operator's default region, and real S3 refuses a request signed for the wrong one. The credential scope names the region the request was signed for.
func TestRegionQueryOverridesTheAmbientRegion(t *testing.T) {
	var authorization atomic.Value

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		authorization.Store(r.Header.Get("Authorization"))
	}))
	t.Cleanup(server.Close)

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")

	opts, err := Parse("s3://cas/team?region=us-west-2&endpoint=" + server.URL)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	store, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = store.HasTree(context.Background(), "probe")
	if err != nil {
		t.Fatalf("HasTree: %v", err)
	}

	credential, _ := authorization.Load().(string)
	if !strings.Contains(credential, "/us-west-2/s3/") {
		t.Fatalf("credential scope %q, want the ?region= override us-west-2", credential)
	}
}

// TestTransportTimeoutsAreBounded: zero on any of these means wait forever, and the blob mirror runs outside every step's timeout, so an endpoint that accepts and then says nothing hangs the build after the step reported success.
func TestTransportTimeoutsAreBounded(t *testing.T) {
	store, _ := newTestStore(t)

	transport, ok := store.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", store.http.Transport)
	}

	for name, timeout := range map[string]time.Duration{
		"ResponseHeaderTimeout": transport.ResponseHeaderTimeout,
		"TLSHandshakeTimeout":   transport.TLSHandshakeTimeout,
		"IdleConnTimeout":       transport.IdleConnTimeout,
	} {
		if timeout <= 0 {
			t.Errorf("%s = %v, want a positive bound", name, timeout)
		}
	}
}

// TestA5xxIsRetriedAndA4xxIsNot: S3 sheds load with 503 SlowDown and expects the client back; a refusal is final, and retrying one only delays the error. The exceptions are the SDK standard retryer's, which this replaced: a 400 RequestTimeout (an idle upload socket) and a 429 are transient, a 501 is not.
func TestA5xxIsRetriedAndA4xxIsNot(t *testing.T) {
	var (
		calls  atomic.Int32
		answer atomic.Value
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			first, _ := answer.Load().([2]string)
			status, _ := strconv.Atoi(first[0])
			w.WriteHeader(status)
			_, _ = io.WriteString(w, first[1])

			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	opts, err := Parse("s3://cas/team?endpoint=" + server.URL + "&region=us-east-1")
	if err != nil {
		t.Fatal(err)
	}

	store, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		status, code string
		retried      bool
	}{
		{"503", "SlowDown", true},
		{"500", "InternalError", true},
		{"429", "SlowDown", true},
		{"400", "RequestTimeout", true},
		{"403", "AccessDenied", false},
		{"400", "InvalidRequest", false},
		{"501", "NotImplemented", false},
	} {
		calls.Store(0)
		answer.Store([2]string{tc.status, "<Error><Code>" + tc.code + "</Code></Error>"})

		err = store.PutTree(context.Background(), "k", t.TempDir())

		// Retried: the second call's 200 is the answer. Not: the first call's refusal is.
		want := map[bool]int32{true: 2, false: 1}[tc.retried]
		if (err == nil) != tc.retried || calls.Load() != want {
			t.Errorf("after one %s %s: err=%v calls=%d, want %d calls", tc.status, tc.code, err, calls.Load(), want)
		}
	}
}

// TestRequestsAreSentAsS3VerifiesThem is minioAwkwardUploads without minio: the path S3 re-encodes before checking the signature must already be in that encoding, and a body must say its length.
func TestRequestsAreSentAsS3VerifiesThem(t *testing.T) {
	var (
		mu      sync.Mutex
		paths   []string
		chunked bool
	)

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		paths = append(paths, r.RequestURI)
		chunked = chunked || len(r.TransferEncoding) > 0
	}))
	t.Cleanup(server.Close)

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	opts, err := Parse("s3://cas/team?endpoint=" + server.URL + "&region=us-east-1")
	if err != nil {
		t.Fatal(err)
	}

	store, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}

	err = store.PutTree(context.Background(), "a+b=c", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(paths) != 1 || paths[0] != "/cas/team/blobs/a%2Bb%3Dc" || chunked {
		t.Fatalf("sent %v (chunked=%v), want /cas/team/blobs/a%%2Bb%%3Dc with a Content-Length", paths, chunked)
	}
}
