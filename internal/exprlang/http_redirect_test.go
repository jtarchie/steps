package exprlang

import (
	"context"
	"fmt"
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
