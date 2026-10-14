package web

import (
	"html"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// TestTheMCPTabListsWhatIsShared: an export with nobody connected says so, and one with a connection and a call counts both.
func TestTheMCPTabListsWhatIsShared(t *testing.T) {
	t.Parallel()

	server, accounts := exportServer(t)

	before := html.UnescapeString(serve(t, server, http.MethodGet, "/p/ops/mcp", "", "alex").Body.String())
	if !strings.Contains(before, `<a href="/p/ops/mcp/exports/support">support</a>`) || !strings.Contains(before, "nobody") {
		t.Fatalf("the mcp tab does not list support with nobody connected:\n%s", before)
	}

	connect(t, server, accounts)

	after := html.UnescapeString(serve(t, server, http.MethodGet, "/p/ops/mcp", "", "alex").Body.String())
	if !strings.Contains(after, "1 person<span class=\"dim\"> · 1 agent</span>") || !strings.Contains(after, `<td class="num">1</td>`) {
		t.Errorf("the mcp tab does not count alex's connection and call:\n%s", after)
	}
}

// TestTheExportPageSaysHowToConnectAndWhatItOffers: the address an agent is given, how each agent takes it, the rule, the fixed arguments, and what was called.
func TestTheExportPageSaysHowToConnectAndWhatItOffers(t *testing.T) {
	t.Parallel()

	server, accounts := exportServer(t)
	connect(t, server, accounts)

	page := html.UnescapeString(serve(t, server, http.MethodGet, "/p/ops/mcp/exports/support", "", "alex").Body.String())

	for _, want := range []string{
		`data-copy="https://steps.example/mcp/x/ops/support"`,
		`claude mcp add --transport http support https://steps.example/mcp/x/ops/support`,
		`<code>UserID == "alex"</code>`,
		`<code>owner=acme</code>`,
		`20s limit`,
		`mcp call`,
		`<a href="/p/ops/mcp">mcp</a>`,
	} {
		if !strings.Contains(page, want) && !strings.Contains(stripTags(page), want) {
			t.Errorf("the export page does not carry %s", want)
		}
	}

	if code := serve(t, server, http.MethodGet, "/p/ops/mcp/exports/nothing", "", "alex").Code; code != http.StatusNotFound {
		t.Errorf("an export that does not exist = %d, want 404", code)
	}
}

func TestTheCopyButtonSaysItCopied(t *testing.T) {
	t.Parallel()

	js, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{`closest('[data-copy]')`, `navigator.clipboard.writeText`, `'copied'`, `'address copied'`} {
		if !strings.Contains(string(js), want) {
			t.Errorf("app.js does not carry %s", want)
		}
	}
}

// connect gives alex a live grant on support from a registered client, and records one call under it.
func connect(t *testing.T, server *Server, accounts store.Accounts) {
	t.Helper()

	client := register(t, server, "https://agent.example/cb")
	grant := store.Grant{ClientID: client, Actor: "local:alex", Pipeline: "ops", Export: "support"}

	err := accounts.IssueToken(t.Context(), store.OAuthToken{TokenHash: "connected", Kind: "access", Grant: grant, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	server.recordGrantActivity(t.Context(), server.Lookup("ops"), grant, "mcp call", `{"tool":"github__search_issues"}`)
}

// TestTheExportPageShowsOnlyAgentActivity: a job sharing the export's name is a different thing, and triggering it is not a call to the export.
func TestTheExportPageShowsOnlyAgentActivity(t *testing.T) {
	t.Parallel()

	server, accounts := exportServer(t)
	connect(t, server, accounts)

	err := server.Lookup("ops").Store.RecordActivity(t.Context(), store.ActivityEntry{Actor: "local:alex", Action: "trigger", Target: "support"}, 0)
	if err != nil {
		t.Fatal(err)
	}

	page := stripTags(serve(t, server, http.MethodGet, "/p/ops/mcp/exports/support", "", "alex").Body.String())
	if !strings.Contains(page, "mcp call") || strings.Contains(page, "trigger") {
		t.Errorf("the export page should list the agent's call and not a job's trigger:\n%s", page)
	}
}
