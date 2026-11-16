package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// miscFixtureWords are names the fixtures gave these pages and the vendors' own product names: data, never chrome.
var miscFixtureWords = []string{
	"ops", "support", "alex", "bob", "linear", "gopls", "perstep", "triager", "owner", "acme", "search", "issues", "agent", "example",
	"Anthropic", "OpenAI", "dddd", "addresscopied", "ago", "copied", "data", "expired", "running", "signedout", "api", "hub", "hubcopilot", "cwd", "sh", "USERPROFILE", "such", "Claude", "Code", "Cursor", "VS", "Codex", "Gemini", "CLI", "Windsurf", "Zed", "ChatGPT", "OAuth", "claude", "sam", "not", "today",
	"local", "approval", "git", "gone",
}

var mcpTargetTitle = regexp.MustCompile(`class="mcp-meta" title="[^"]*"`)

func untranslatedMisc(page, fixture string) []string {
	page = mcpTargetTitle.ReplaceAllString(page, `class="mcp-meta"`)

	for _, word := range miscFixtureWords {
		page = strings.ReplaceAll(page, word, "")
	}

	return untranslated(page, fixture)
}

func pseudoServe(t *testing.T, server *Server, target, as string) string {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Accept-Language", pseudoTag)

	if as != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: as, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Body.String()
}

func assertSpeaksCatalog(t *testing.T, name, page string) {
	t.Helper()

	if !strings.Contains(page, `<html lang="en-XA">`) {
		t.Errorf("%s: not drawn in the pseudo-locale:\n%.400s", name, page)

		return
	}

	if left := untranslatedMisc(page, t.Name()); len(left) > 0 {
		t.Errorf("%s: plain English outside the catalog: %v", name, slices.Compact(slices.Sorted(slices.Values(left))))
	}
}

func TestTheMCPLoginActivityAndConnectionPagesSpeakThroughTheCatalog(t *testing.T) {
	t.Parallel()

	server, accounts := exportServer(t)
	server.pseudo = true
	connect(t, server, accounts)

	pipeline := server.Lookup("ops")

	for _, entry := range []store.ActivityEntry{
		{Actor: "local:alex", Action: "trigger", Target: "build"},
		{Action: "pause"},
		{Actor: "local:sam", Action: "reject", Target: "approval 3", Detail: `{"reason":"not today"}`},
	} {
		err := pipeline.Store.RecordActivity(context.Background(), entry, 0)
		if err != nil {
			t.Fatal(err)
		}
	}

	err := pipeline.Store.RecordRevision(context.Background(), "dddd111122223333", "jobs: []\n", nil)
	if err != nil {
		t.Fatal(err)
	}

	register := register(t, server, "https://agent.example/cb")

	for _, path := range []string{
		"/p/ops/mcp", "/p/ops/mcp/exports/support", "/connections", "/activity", "/activity?actor=local%3Aalex",
		"/p/ops/config/dddd111122223333", "/p/ops/jobs/build/follow", authorizeURL(register, "https://agent.example/cb"),
	} {
		assertSpeaksCatalog(t, path, pseudoServe(t, server, path, "alex"))
	}

	assertSpeaksCatalog(t, "/login", pseudoServe(t, server, "/login", ""))
	assertSpeaksCatalog(t, "/oauth/authorize (unknown client)", pseudoServe(t, server, "/oauth/authorize?"+url.Values{"client_id": {"nope"}}.Encode(), "alex"))
}

func TestTheMCPServersTableSpeaksThroughTheCatalog(t *testing.T) {
	t.Parallel()

	server, _, _ := mcpServerPipeline(t, stubRunner{})
	server.pseudo = true

	assertSpeaksCatalog(t, "/p/demo/mcp", pseudoServe(t, server, "/p/demo/mcp", ""))
}

// TestTheFollowScriptDrawsItsWordsFromTheServer: the follow page's script and the checks it polls for are in the visitor's locale too.
func TestTheFollowScriptDrawsItsWordsFromTheServer(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)
	server.pseudo = true

	page := pseudoServe(t, server, "/p/demo/jobs/build/follow", "")
	if strings.Contains(page, "'lost contact with the server'") || !strings.Contains(page, "var W = ") {
		t.Error("the follow script still carries its own English")
	}
}
