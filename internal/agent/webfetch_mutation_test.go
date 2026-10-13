package agent

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// webfetchRedirectChain serves /N as a redirect to /N-1, and /0 as the landing page, so /N costs exactly N redirects.
func webfetchRedirectChain(t *testing.T) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil || n == 0 {
			_, _ = w.Write([]byte("landed"))

			return
		}

		http.Redirect(w, r, "/"+strconv.Itoa(n-1), http.StatusFound)
	}))
	t.Cleanup(server.Close)

	return server
}

// TestWebFetchRedirectLimitMatchesNetHTTP pins the cap installing CheckRedirect replaced: net/http's default counts the original request in via, so nine redirects land and the tenth is refused.
func TestWebFetchRedirectLimitMatchesNetHTTP(t *testing.T) {
	t.Parallel()

	server := webfetchRedirectChain(t)

	result := fetchResult(t, []string{"127.0.0.1"}, map[string]any{"url": server.URL + "/" + strconv.Itoa(maxWebFetchRedirects-1)})
	if result["body"] != "landed" {
		t.Errorf("%d redirects: result = %v, want them followed", maxWebFetchRedirects-1, result)
	}

	result = fetchResult(t, []string{"127.0.0.1"}, map[string]any{"url": server.URL + "/" + strconv.Itoa(maxWebFetchRedirects)})
	if got, _ := result["error"].(string); !strings.Contains(got, "stopped after") {
		t.Errorf("%d redirects: result = %v, want the chain refused", maxWebFetchRedirects, result)
	}
}

func TestWebFetchBodyExactlyAtCapIsNotTruncated(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, maxWebFetchBytes))
	}))
	defer server.Close()

	result := fetchResult(t, []string{"127.0.0.1"}, map[string]any{"url": server.URL})

	if body, _ := result["body"].(string); len(body) != maxWebFetchBytes || result["truncated"] != false {
		t.Errorf("body length = %d, truncated = %v; want the whole body with truncated false", len(body), result["truncated"])
	}
}
