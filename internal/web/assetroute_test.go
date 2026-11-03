package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAssetRoutesServeTheirOwnFileWithItsContentHashAsETag: four routes share one handler factory, so a swapped file or content type between them is the bug, and the tag must stay the content's own hash so a vendored bump invalidates it.
func TestAssetRoutesServeTheirOwnFileWithItsContentHashAsETag(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	for route, want := range map[string]struct{ file, contentType string }{
		"/static/app.css":       {"static/app.css", "text/css; charset=utf-8"},
		"/static/htmx.min.js":   {"static/htmx.min.js", "text/javascript; charset=utf-8"},
		"/static/hx-sse.min.js": {"static/hx-sse.min.js", "text/javascript; charset=utf-8"},
		"/static/app.js":        {"static/app.js", "text/javascript; charset=utf-8"},
	} {
		data, err := assets.ReadFile(want.file)
		if err != nil {
			t.Fatal(err)
		}

		sum := sha256.Sum256(data)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`

		// Twice: the second request is the one a cached tag would serve.
		for range 2 {
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, route, nil))

			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != want.contentType {
				t.Errorf("%s: %d %q, want 200 %q", route, rec.Code, rec.Header().Get("Content-Type"), want.contentType)
			}

			if rec.Header().Get("ETag") != etag || rec.Header().Get("Cache-Control") != "no-cache" {
				t.Errorf("%s: ETag %q Cache-Control %q, want %q no-cache", route, rec.Header().Get("ETag"), rec.Header().Get("Cache-Control"), etag)
			}

			if rec.Body.String() != string(data) {
				t.Errorf("%s: body is not the embedded %s", route, want.file)
			}
		}

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, route, nil)
		req.Header.Set("If-None-Match", etag)

		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusNotModified {
			t.Errorf("%s: revalidating with its tag gave %d, want 304", route, rec.Code)
		}
	}
}
