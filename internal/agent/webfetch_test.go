package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func fetchResult(t *testing.T, allow []string, args map[string]any) map[string]any {
	t.Helper()

	_, impl := webFetchTool(allow)

	return impl(context.Background(), args, toolEnv{})
}

func TestWebFetchReturnsBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello from the spec"))
	}))
	defer server.Close()

	result := fetchResult(t, nil, map[string]any{"url": server.URL})

	if result["status"] != http.StatusOK || result["body"] != "hello from the spec" || result["truncated"] != false {
		t.Errorf("result = %v, want status 200 with the body untruncated", result)
	}

	if ct, _ := result["content_type"].(string); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content_type = %v, want text/plain", result["content_type"])
	}
}

func TestWebFetchTruncatesAtCap(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, maxWebFetchBytes+1))
	}))
	defer server.Close()

	result := fetchResult(t, nil, map[string]any{"url": server.URL})

	if body, _ := result["body"].(string); len(body) != maxWebFetchBytes || result["truncated"] != true {
		t.Errorf("body length = %d, truncated = %v; want the cap with truncated true", len(result["body"].(string)), result["truncated"])
	}
}

func TestWebFetchArgumentAndSchemeErrors(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		args map[string]any
		want string
	}{
		"missing url":      {map[string]any{}, `missing required argument "url"`},
		"non-http scheme":  {map[string]any{"url": "file:///etc/passwd"}, "unsupported scheme"},
		"schemeless value": {map[string]any{"url": "not a url"}, "unsupported scheme"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			result := fetchResult(t, nil, tc.args)
			if got, _ := result["error"].(string); !strings.Contains(got, tc.want) {
				t.Errorf("error = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestWebFetchRefusesHostOutsideAllow(t *testing.T) {
	t.Parallel()

	// No server: the refusal must happen before any connection is attempted,
	// so an unreachable host proves the check runs first.
	result := fetchResult(t, []string{"specification.website"}, map[string]any{"url": "https://untrusted.example/creds"})

	got, _ := result["error"].(string)
	if !strings.Contains(got, `"untrusted.example"`) || !strings.Contains(got, "allow") {
		t.Errorf("error = %q, want a refusal naming the host and the allow list", got)
	}
}

// TestWebFetchRefusesRedirectOffAllowlist is the hole an origin-only check
// would leave: a permitted host answering 302 to anywhere. CheckRedirect runs
// before the next hop is dialed, so the disallowed target needs no server —
// reaching DNS at all would already be the bug.
func TestWebFetchRefusesRedirectOffAllowlist(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://untrusted.example/creds", http.StatusFound)
	}))
	defer server.Close()

	result := fetchResult(t, []string{"127.0.0.1"}, map[string]any{"url": server.URL})

	if got, _ := result["error"].(string); !strings.Contains(got, `"untrusted.example"`) {
		t.Errorf("error = %q, want the redirect target refused by the allow list", got)
	}
}

func TestWebFetchFollowsRedirectWithinAllowlist(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/b", http.StatusFound) })
	mux.HandleFunc("/b", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("landed")) })

	server := httptest.NewServer(mux)
	defer server.Close()

	result := fetchResult(t, []string{"127.0.0.1"}, map[string]any{"url": server.URL + "/a"})

	if result["body"] != "landed" {
		t.Errorf("result = %v, want the redirect within the allow list followed", result)
	}
}

func TestCheckWebFetchHost(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		host    string
		allow   []string
		refused bool
	}{
		"empty list allows anything": {"anything.example", nil, false},
		"exact match":                {"specification.website", []string{"specification.website"}, false},
		"subdomain match":            {"www.specification.website", []string{"specification.website"}, false},
		"case-insensitive":           {"Specification.WEBSITE", []string{"specification.website"}, false},
		"suffix is not a subdomain":  {"evilspecification.website", []string{"specification.website"}, true},
		"unrelated host":             {"example.com", []string{"specification.website"}, true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := checkWebFetchHost(&url.URL{Scheme: "https", Host: tc.host}, tc.allow)
			if refused := err != nil; refused != tc.refused {
				t.Errorf("checkWebFetchHost(%q, %v) error = %v, want refused=%v", tc.host, tc.allow, err, tc.refused)
			}
		})
	}
}

// TestWebFetchDeclarationCarriesAllow pins that the model is told about the
// fence up front, instead of discovering it one refused call at a time.
func TestWebFetchDeclarationCarriesAllow(t *testing.T) {
	t.Parallel()

	decl, _ := webFetchTool([]string{"specification.website"})
	if !strings.Contains(decl.Description, "specification.website") {
		t.Errorf("declaration description = %q, want it to name the allowed host", decl.Description)
	}

	bare, _ := webFetchTool(nil)
	if strings.Contains(bare.Description, "Restricted") {
		t.Errorf("bare-grant description = %q, want no restriction claim", bare.Description)
	}
}
