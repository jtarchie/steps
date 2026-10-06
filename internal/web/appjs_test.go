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

// TestGraphTraceIsDelegatedAndSurvivesAPoll: hovering or focusing a graph
// node lifts its edges and neighbours. The graph is morphed every 2.5s, so a
// listener bound to the nodes would miss the ones that arrive later, and the
// classes the trace adds are the server's to take back on every swap — so the
// listeners sit on the document and the trace is re-applied after a swap.
// No JS runtime runs in this suite; Firefox against a real daemon is the
// behavioural check (see .design/home-and-graph/TASKS.md task 5).
func TestGraphTraceIsDelegatedAndSurvivesAPoll(t *testing.T) {
	t.Parallel()

	js, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}

	script := string(js)

	for _, want := range []string{
		"document.addEventListener('mouseover', function (e) { traceFrom(e.target); });",
		"document.addEventListener('focusin', function (e) { traceFrom(e.target); });",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}

	_, swap, found := strings.Cut(script, "document.addEventListener('htmx:after:swap', function () {")
	if !found {
		t.Fatal("app.js has no after-swap handler")
	}

	if body, _, _ := strings.Cut(swap, "});"); !strings.Contains(body, "trace();") {
		t.Errorf("the after-swap handler does not re-apply the trace:\n%s", body)
	}
}
