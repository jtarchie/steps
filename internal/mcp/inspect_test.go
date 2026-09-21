package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

const inspectEndpoint = "https://tracker.example/mcp"

// What a page is allowed to say about a saved credential. The checks are the ones a RUN makes, so the two cannot disagree about whether a server works — only about the wording, because a run's error names the login command to type and a page has a button for it.
func TestInspectTokenReadsTheFileAndNothingElse(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))

	srv := config.MCPServer{Name: "tracker", Endpoint: inspectEndpoint, Auth: config.MCPServerAuth{Type: "oauth"}}

	const secret = "tok-must-never-be-rendered"

	for _, test := range []struct {
		name      string
		token     *TokenFile
		connected bool
		renews    bool
		detail    string
	}{
		{
			name:   "no login was ever done",
			detail: "needs login",
		},
		{
			name:   "a token issued for a different endpoint",
			token:  &TokenFile{Endpoint: "https://elsewhere.example/mcp", AccessToken: secret, RefreshToken: "r"},
			detail: "different endpoint",
		},
		{
			name:   "an expired token with nothing to renew it",
			token:  &TokenFile{Endpoint: inspectEndpoint, AccessToken: secret, Expiry: time.Now().Add(-time.Hour)},
			detail: "expired",
		},
		{
			name:      "a token that renews itself",
			token:     &TokenFile{Endpoint: inspectEndpoint, AccessToken: secret, RefreshToken: "r"},
			connected: true,
			renews:    true,
			detail:    "renews automatically",
		},
		{
			// It works right now, which is all a run needs — and it is the one state nobody gets a second warning about, so the page says when it dies.
			name:      "a token that works and cannot renew itself",
			token:     &TokenFile{Endpoint: inspectEndpoint, AccessToken: secret, Expiry: time.Now().Add(time.Hour)},
			connected: true,
			detail:    "cannot renew itself",
		},
		{
			name:      "a token with no expiry at all",
			token:     &TokenFile{Endpoint: inspectEndpoint, AccessToken: secret},
			connected: true,
			detail:    "connected",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			saveTokenFile(t, srv.Name, test.token)

			got := InspectToken(srv)
			if got.Connected != test.connected || got.Renews != test.renews {
				t.Errorf("%+v, want connected=%v renews=%v", got, test.connected, test.renews)
			}

			if !strings.Contains(got.Detail, test.detail) {
				t.Errorf("detail = %q, want it to say %q", got.Detail, test.detail)
			}

			// An operator has to be able to find, inspect or delete the file this is about.
			if got.Path == "" {
				t.Error("the state does not say where the token lives")
			}

			// A page renders this, and the credential is the one thing that must never reach one.
			if strings.Contains(got.Detail, secret) {
				t.Errorf("the credential state carries the credential: %q", got.Detail)
			}
		})
	}
}

// saveTokenFile writes one saved credential where a login would have left it, or removes it for nil.
func saveTokenFile(t *testing.T, server string, token *TokenFile) {
	t.Helper()

	path, err := TokenPath(server)
	if err != nil {
		t.Fatal(err)
	}

	err = os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	if token == nil {
		err = os.Remove(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}

		return
	}

	body, err := json.Marshal(token) //nolint:gosec // G117: writing the token file IS this fixture's job; the struct is the format under test and every value in it is invented here
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, body, 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// A login a PAGE started has a reader looking at a page, and BOTH ways the provider can answer have to put them back on it: the exchange happens after this handler returns, so the failure that matters most is reported by the page and not here.
func TestAHostedCallbackReturnsTheReaderToThePageThatStartedIt(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		query string
	}{
		{name: "the provider approved", query: "?code=c&state=minted"},
		{name: "the provider refused", query: "?error=access_denied&state=minted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			hosted := NewHostedCallback("http://daemon.test/mcp/callback", "/p/app/mcp", func(string) {})
			hosted.cb.expect("minted")

			rec := httptest.NewRecorder()
			hosted.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/mcp/callback"+test.query, nil))

			if rec.Code != http.StatusSeeOther {
				t.Errorf("the callback answered %d, want a 303 back to the page", rec.Code)
			}

			if got := rec.Header().Get("Location"); got != "/p/app/mcp" {
				t.Errorf("the callback sent the reader to %q, want the page the login started from", got)
			}

			if got := hosted.ReturnsTo(); got != "/p/app/mcp" {
				t.Errorf("ReturnsTo = %q, want the page the login started from", got)
			}
		})
	}
}

// A login a TERMINAL started has nobody at a browser to send anywhere: the CLI polling for it is what reports the outcome, and a redirect would send the reader to a daemon page they never asked for.
func TestATerminalCallbackKeepsItsPlainSentence(t *testing.T) {
	t.Parallel()

	hosted := NewHostedCallback("http://daemon.test/mcp/callback", "", func(string) {})
	hosted.cb.expect("minted")

	rec := httptest.NewRecorder()
	hosted.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/mcp/callback?code=c&state=minted", nil))

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "close this window") {
		t.Errorf("a terminal login's callback = %d %q, want the sentence it has always printed", rec.Code, rec.Body.String())
	}

	if got := hosted.ReturnsTo(); got != "" {
		t.Errorf("ReturnsTo = %q, want nothing for a login no page started", got)
	}
}
