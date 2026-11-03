package exprlang

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func redirectSource(url string) string {
	return fmt.Sprintf(`[http({url: %q, headers: {"X-Api-Key": "secret"}})] | map(({a: "b"}))`, url)
}

// keyServer reports the X-Api-Key each request arrived with on the returned channel.
func keyServer(t *testing.T, mux *http.ServeMux) (*httptest.Server, chan string) {
	t.Helper()

	seen := make(chan string, 1)

	mux.HandleFunc("/landed", func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("X-Api-Key")

		_, _ = w.Write([]byte(`{}`))
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server, seen
}

func TestHTTPRedirectKeepsCustomHeadersOnSameHost(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/landed", http.StatusFound)
	})

	server, seen := keyServer(t, mux)

	_, err := RunCheck(context.Background(), redirectSource(server.URL+"/start"), Input{})
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	if got := <-seen; got != "secret" {
		t.Errorf("X-Api-Key after same-host redirect = %q, want it kept", got)
	}
}

func TestHTTPRedirectDropsCustomHeadersAcrossHosts(t *testing.T) {
	t.Parallel()

	target, seen := keyServer(t, http.NewServeMux())
	// localhost vs 127.0.0.1: a different host as well as a different port
	elsewhere := strings.Replace(target.URL, "127.0.0.1", "localhost", 1) + "/landed"

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere, http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	_, err := RunCheck(context.Background(), redirectSource(origin.URL), Input{})
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	if got := <-seen; got != "" {
		t.Errorf("X-Api-Key after cross-host redirect = %q, want it dropped", got)
	}
}

func TestHTTPRedirectLoopStopsAtTheLimit(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	t.Cleanup(server.Close)

	_, err := RunCheck(context.Background(), redirectSource(server.URL), Input{})
	if err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Errorf("err = %v, want a stopped-after-redirects error", err)
	}
}

func TestHTTPErrorTextOmitsTheQueryString(t *testing.T) {
	t.Parallel()

	server := jsonServer(t)
	dead := server.URL
	server.Close()

	_, err := RunCheck(context.Background(), fmt.Sprintf(`[http({url: %q, query: {token: "s3cret"}})] | map(({a: "b"}))`, dead), Input{})
	if err == nil {
		t.Fatal("RunCheck: want an error")
	}

	if strings.Contains(err.Error(), "?token=") || !strings.Contains(err.Error(), "GET "+dead) {
		t.Errorf("err = %v, want the request named without its query", err)
	}
}

func TestHTTPRedirectDropsBodyAcrossHosts(t *testing.T) {
	t.Parallel()

	type landed struct {
		body, key, contentType string
		length                 int64
	}

	seen := make(chan landed, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		seen <- landed{string(data), r.Header.Get("X-Api-Key"), r.Header.Get("Content-Type"), r.ContentLength}

		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(target.Close)

	elsewhere := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)

	source := fmt.Sprintf(`[http({url: %q, method: "POST", headers: {"X-Api-Key": "secret"}, json: {token: "hunter2"}})] | map(({a: "b"}))`, origin.URL)

	_, err := RunCheck(context.Background(), source, Input{})
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	got := <-seen
	if got.body != "" || got.length > 0 {
		t.Errorf("body after cross-host 307 = %q (length %d), want none", got.body, got.length)
	}

	if got.key != "" || got.contentType != "" {
		t.Errorf("headers after cross-host 307 = key %q, content-type %q, want none", got.key, got.contentType)
	}
}
