package web

import (
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"regexp"
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
		`claude mcp add --transport http --scope user support https://steps.example/mcp/x/ops/support`,
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

// TestTheExportPageWalksEachAppThroughConnecting: what a reader copies out of the page — a command, a one-click link, a config block — carries THIS export's address and name, because a wrong one fails inside another application with nothing pointing back here.
func TestTheExportPageWalksEachAppThroughConnecting(t *testing.T) {
	t.Parallel()

	server, _ := exportServer(t)

	const address = "https://steps.example/mcp/x/ops/support"

	body := serve(t, server, http.MethodGet, "/p/ops/mcp/exports/support", "", "alex").Body.String()
	page := html.UnescapeString(body)

	for _, want := range []string{
		"claude mcp add --transport http --scope user support " + address,
		"codex mcp add support --url " + address,
		"Using support, list the tools you have.",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the export page does not carry %q", want)
		}
	}

	assertCursorLink(t, page, address)
	assertVSCodeLink(t, page, address)
	assertConfigBlocks(t, body, address)
	assertTabsAreRadioLabelPanel(t, body, address)
}

func assertCursorLink(t *testing.T, page, address string) {
	t.Helper()

	cursor := regexp.MustCompile(`href="(cursor://[^"]+)"`).FindStringSubmatch(page)
	if cursor == nil {
		t.Fatal("the export page has no one-click Cursor link")
	}

	_, encoded, _ := strings.Cut(cursor[1], "config=")

	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("the Cursor link's config is not base64: %v", err)
	}

	var cursorConfig map[string]string

	err = json.Unmarshal(raw, &cursorConfig)
	if err != nil || cursorConfig["url"] != address || !strings.Contains(cursor[1], "name=support&") {
		t.Errorf("the Cursor link installs %s as %q, want %s as support", raw, cursor[1], address)
	}
}

func assertVSCodeLink(t *testing.T, page, address string) {
	t.Helper()

	vscode := regexp.MustCompile(`href="vscode:mcp/install\?([^"]+)"`).FindStringSubmatch(page)
	if vscode == nil {
		t.Fatal("the export page has no one-click VS Code link")
	}

	query, err := url.QueryUnescape(vscode[1])
	if err != nil {
		t.Fatal(err)
	}

	var vscodeConfig map[string]string

	err = json.Unmarshal([]byte(query), &vscodeConfig)
	if err != nil || vscodeConfig["url"] != address || vscodeConfig["name"] != "support" {
		t.Errorf("the VS Code link installs %s, want %s as support", query, address)
	}
}

func assertConfigBlocks(t *testing.T, body, address string) {
	t.Helper()

	blocks := 0

	for _, pre := range regexp.MustCompile(`(?s)<pre[^>]*>(.*?)</pre>`).FindAllStringSubmatch(body, -1) {
		snippet := html.UnescapeString(pre[1])
		if !strings.HasPrefix(snippet, "{") {
			continue
		}

		blocks++

		var parsed map[string]map[string]any

		err := json.Unmarshal([]byte(snippet), &parsed)
		if err != nil {
			t.Errorf("a config block does not parse: %v\n%s", err, snippet)

			continue
		}

		for _, servers := range parsed {
			if _, ok := servers["support"]; !ok || !strings.Contains(snippet, address) {
				t.Errorf("a config block does not name support at %s:\n%s", address, snippet)
			}
		}
	}

	if blocks == 0 {
		t.Error("the export page has no config block to paste")
	}
}

// assertTabsAreRadioLabelPanel: the tabs are CSS alone, so the markup's shape IS the behaviour — every panel starts hidden, and only a checked radio followed by its label and its panel reveals one.
func assertTabsAreRadioLabelPanel(t *testing.T, body, address string) {
	t.Helper()

	tabs := regexp.MustCompile(`<input class="visually-hidden connect-radio" type="radio" name="connect-app" id="app-([a-z-]+)"( checked)?>\s*<label class="connect-tab" for="app-([a-z-]+)">[^<]+</label>\s*<div class="connect-panel"`).FindAllStringSubmatch(body, -1)

	clients, checked := 0, 0
	for _, group := range connectGroups("support", address) {
		clients += len(group.Clients)
	}

	for _, tab := range tabs {
		if tab[1] != tab[3] {
			t.Errorf("the radio app-%s is labelled for app-%s", tab[1], tab[3])
		}

		if tab[2] != "" {
			checked++
		}
	}

	if len(tabs) != clients || checked != 1 {
		t.Errorf("%d of %d tabs are radio, label, panel in that order, %d checked; want all, one checked", len(tabs), clients, checked)
	}

	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(css), ".connect-radio:checked + .connect-tab + .connect-panel { display: block; }") {
		t.Error("app.css no longer reveals the panel after a checked radio and its label")
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

	err := accounts.IssueToken(t.Context(), store.OAuthToken{TokenHash: "connected", Kind: store.TokenAccess, Grant: grant, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
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

// TestAnAppThatCannotReachTheAddressIsNotOffered: Claude and ChatGPT connect from their vendors' servers, so on an address only this machine or this network answers, their steps would fail inside the app with nothing pointing back here.
func TestAnAppThatCannotReachTheAddressIsNotOffered(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		external        string
		claude, chatgpt bool
	}{
		{"http://127.0.0.1:8088", false, false},
		{"http://localhost:8088", false, false},
		{"https://10.0.0.5", false, false},
		{"http://steps.example", true, false},
		{"https://steps.example", true, true},
	} {
		t.Run(tc.external, func(t *testing.T) {
			t.Parallel()

			server, _ := exportServer(t, WithExternalURL(tc.external))
			page := html.UnescapeString(serve(t, server, http.MethodGet, "/p/ops/mcp/exports/support", "", "alex").Body.String())

			assertTabOffered(t, page, tc.external, "claude", tc.claude)
			assertTabOffered(t, page, tc.external, "chatgpt", tc.chatgpt)

			if blocked := !tc.claude || !tc.chatgpt; blocked != strings.Contains(page, "--external-url") {
				t.Errorf("on %s the page should name --external-url exactly when a tab is disabled", tc.external)
			}

			if tc.claude && !strings.Contains(page, "must be reachable from the internet") {
				t.Errorf("on %s Claude is offered without saying the address must be public", tc.external)
			}
		})
	}
}

func assertTabOffered(t *testing.T, page, external, slug string, offered bool) {
	t.Helper()

	radio := regexp.MustCompile(`<input class="visually-hidden connect-radio" type="radio" name="connect-app" id="app-` + slug + `"([^>]*)>`).FindStringSubmatch(page)
	if radio == nil {
		t.Fatalf("no %s tab", slug)
	}

	if disabled := strings.Contains(radio[1], "disabled"); disabled == offered {
		t.Errorf("%s on %s: disabled=%v, want %v", slug, external, disabled, !offered)
	}

	if !offered && (!strings.Contains(radio[1], `aria-describedby="blocked-`+slug+`"`) || !regexp.MustCompile(`<p id="blocked-`+slug+`">[^<]+</p>`).MatchString(page)) {
		t.Errorf("%s on %s is disabled without saying why", slug, external)
	}
}
