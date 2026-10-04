package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// TestPagesAndAssetsAreGzipped: every polling page re-fetches itself every 2.5s, so an uncompressed page is ~3.5x the bytes on every poll of every open tab.
func TestPagesAndAssetsAreGzipped(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	for _, path := range []string{"/p/demo", "/p/demo/runs", "/docs/README.md", "/static/app.css", "/static/htmx.min.js"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		req.Header.Set("Accept-Encoding", "gzip, br")
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}

		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Errorf("%s: Content-Encoding %q, want gzip", path, got)

			continue
		}

		reader, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}

		plain, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}

		if len(plain) == 0 || len(plain) <= rec.Body.Len() {
			t.Errorf("%s: decompressed to %d bytes, not a compressed body", path, len(plain))
		}
	}
}

// TestAssetRevalidationStaysEmptyUnderGzip: a 304 has no body, and a Content-Encoding on nothing is a response some clients refuse to read.
func TestAssetRevalidationStaysEmptyUnderGzip(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/static/app.css", nil))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/static/app.css", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("If-None-Match", first.Header().Get("ETag"))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Fatalf("status %d, want 304", rec.Code)
	}

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("a 304 carries Content-Encoding %q", got)
	}
}

// TestRunStreamIsNotGzipped: a gzip writer holds bytes until its block fills, so a compressed event stream can sit on a frame the browser should already be drawing.
func TestRunStreamIsNotGzipped(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-plain", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-plain", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "first", StepKind: "task", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "first", StepKind: "task", StepID: 1, Status: "succeeded"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-plain", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)

	go func() {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/p/demo/runs/run-plain/events?after=0", nil)
		// What htmx's sse extension sends on every request, which is why the exemption is by route and not by Accept.
		req.Header.Set("Accept", "text/html, text/event-stream")
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		done <- rec
	}()

	var rec *httptest.ResponseRecorder

	select {
	case rec = <-done:
	case <-time.After(streamHangBound):
		t.Fatal("SSE stream did not close for a finished run")
	}

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("the event stream is Content-Encoding %q", got)
	}

	if !strings.HasPrefix(rec.Body.String(), "id: ") {
		t.Errorf("the event stream is not plain SSE:\n%.200q", rec.Body.String())
	}
}

// TestAssetRangeIsNotGzipped: a range ServeContent computes is offsets into the plain file, but under gzip a client reads them as offsets into the encoded one, so a resumed or stitched download assembles garbage.
func TestAssetRangeIsNotGzipped(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	whole, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/static/app.css", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Range", "bytes=1000-1999")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Range") != "" {
		t.Fatalf("status %d, Content-Range %q: want the whole file", rec.Code, rec.Header().Get("Content-Range"))
	}

	reader, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}

	plain, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(plain, whole) {
		t.Errorf("decompressed to %d bytes, want the %d-byte file", len(plain), len(whole))
	}
}
