package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLayoutScriptIsACachedAsset: inline, the layout's script was half of every page's HTML, re-sent on each navigation and each 2.5s poll; as a file it is revalidated with a 304.
func TestLayoutScriptIsACachedAsset(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, page := get(t, server, "/p/demo")
	if strings.Contains(page, "steps.folds:") {
		t.Error("the layout script is still inline in the page")
	}

	if !strings.Contains(page, `data-search="/p/demo/search"`) {
		t.Error("the palette does not say which pipeline to search, and the script can no longer be templated with it")
	}

	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/static/app.js", nil))

	if first.Code != http.StatusOK || !strings.HasPrefix(first.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("/static/app.js: %d %q", first.Code, first.Header().Get("Content-Type"))
	}

	if strings.Contains(first.Body.String(), "{{") {
		t.Error("app.js still holds a template action, which nothing will ever execute")
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/static/app.js", nil)
	req.Header.Set("If-None-Match", first.Header().Get("ETag"))
	again := httptest.NewRecorder()
	server.Handler().ServeHTTP(again, req)

	if again.Code != http.StatusNotModified {
		t.Errorf("revalidating app.js: %d, want 304", again.Code)
	}
}
