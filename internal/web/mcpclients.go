package web

// How each app takes an export's address, as data the export page renders without knowing which app it is looking at.
//
// The step that loses people is never the address — it is which menu the address goes in — so the menu paths are the content, in each app's own words. They are perishable: every app reshuffles its settings eventually, and they are all here so re-verifying is reading one list. Ported from knowhere's setup page, whose paths were verified 2026-08-11 against each vendor's own documentation; Claude, ChatGPT and Zed re-verified 2026-10-26.

import (
	"encoding/base64"
	"encoding/json"
	"html/template"
	"net"
	"net/url"
	"strings"
)

// connectStep is one numbered instruction: a menu path, a sentence, a button, a block to copy, or a combination.
type connectStep struct {
	Title string
	// Menu is the click path, one label per element, kept apart so the separator stays the template's and the labels stay verbatim.
	Menu []string
	Body string
	// Link is a template.URL because the cursor: and vscode: schemes would otherwise be filtered to #ZgotmplZ, so every value given it is trusted: a constant, or one of the deep-link builders below.
	Link      template.URL
	LinkLabel string
	Snippet   string
}

// connectClient is one tab.
type connectClient struct {
	Slug string
	Name string
	// Caveat is a plan or permission requirement, shown above step one, so a reader meets the wall before following four steps into it.
	Caveat string
	Steps  []connectStep

	// Vendor is whose servers make the connection, for an app that does not connect from the reader's machine; empty for one that does.
	Vendor string
	// NeedsHTTPS is a vendor that refuses a plain http address.
	NeedsHTTPS bool
	// Reach is what the address must be for this app, shown above its steps; Blocked is why it cannot be, which draws the tab disabled.
	Reach   string
	Blocked string
}

// connectGroup sorts the tabs by where the app connects from, which is what decides whether the address can work for it at all.
type connectGroup struct {
	Name    string
	Hint    string
	Clients []connectClient
	// Fix is how to make a blocked tab work, said once however many are blocked.
	Fix string
}

// connectTryIt is the last step of every tab. Success has to be something the reader sees happen — an answer coming back — and listing the tools is a question every export can answer.
func connectTryIt(l *locale, name string) connectStep {
	return connectStep{Title: l.text("connect.try.title"), Body: l.text("connect.try.body"), Snippet: "Using " + name + ", list the tools you have."}
}

// cursorDeepLink is Cursor's one-click install: the config object it would otherwise be hand-fed, base64'd whole. https://cursor.com/docs/mcp
func cursorDeepLink(name, address string) template.URL {
	config, _ := json.Marshal(map[string]string{"type": "http", "url": address}) //nolint:errchkjson // a map of strings cannot fail to marshal

	return template.URL("cursor://anysphere.cursor-deeplink/mcp/install?name=" + url.QueryEscape(name) + "&config=" + //nolint:gosec // both values are encoded here, and the scheme is the point
		base64.StdEncoding.EncodeToString(config))
}

// vscodeDeepLink is VS Code's one-click install: the server object, JSON then URL encoded, as the whole query. Stable only; Insiders takes the by-hand path. https://code.visualstudio.com/docs/agent-customization/mcp-servers
func vscodeDeepLink(name, address string) template.URL {
	config, _ := json.Marshal(map[string]string{"name": name, "type": "http", "url": address}) //nolint:errchkjson // a map of strings cannot fail to marshal

	return template.URL("vscode:mcp/install?" + url.QueryEscape(string(config))) //nolint:gosec // the value is encoded here, and the scheme is the point
}

// configBlock is one named server inside whatever container an app's config file uses, marshalled rather than typed, so a stray brace cannot ship to a reader with no way to tell.
func configBlock(container, name string, entry map[string]any) string {
	body, _ := json.MarshalIndent(map[string]any{container: map[string]any{name: entry}}, "", "  ") //nolint:errchkjson // every entry in this file is strings and lists of strings

	return string(body)
}

// connectGroups is every tab for one export. The server is named after the export alone: clients prefix each tool with it (mcp__<server>__<tool>) under a 64-character cap, and an export's tools are already server__tool.
func connectGroups(name, address string) []connectGroup {
	return connectGroupsIn(englishLocale(), name, address)
}

func connectGroupsIn(l *locale, name, address string) []connectGroup {
	signIn := l.text("connect.signin", name)

	groups := []connectGroup{{
		Name: l.text("connect.group.machine"),
		Hint: l.text("connect.group.machine.hint"),
		Clients: []connectClient{
			{
				Slug: "claude-code",
				Name: "Claude Code",
				Steps: []connectStep{
					{
						Title:   l.text("connect.cc.run"),
						Body:    l.text("connect.cc.run.body"),
						Snippet: "claude mcp add --transport http --scope user " + name + " " + address,
					},
					{
						Title: l.text("connect.cc.auth"),
						Body:  l.text("connect.cc.auth.body", name),
					},
					{Title: l.text("connect.agree"), Body: signIn},
				},
			},
			{
				Slug: "cursor",
				Name: "Cursor",
				Steps: []connectStep{
					{
						Title:     l.text("connect.oneclick"),
						Body:      l.text("connect.cursor.confirm"),
						Link:      cursorDeepLink(name, address),
						LinkLabel: l.text("connect.addto", name, "Cursor"),
					},
					{
						Title:   l.text("connect.byhand"),
						Body:    l.text("connect.cursor.byhand"),
						Snippet: configBlock("mcpServers", name, map[string]any{"type": "http", "url": address}),
					},
					{
						Title: l.text("connect.signin.title"),
						Menu:  []string{"Settings", "MCP"},
						Body:  l.text("connect.cursor.signin", name, signIn),
					},
				},
			},
			{
				Slug: "vscode",
				Name: "VS Code",
				Steps: []connectStep{
					{
						Title:     l.text("connect.oneclick"),
						Body:      l.text("connect.vscode.insiders"),
						Link:      vscodeDeepLink(name, address),
						LinkLabel: l.text("connect.addto", name, "VS Code"),
					},
					{
						Title:   l.text("connect.byhand"),
						Menu:    []string{"Command Palette", "MCP: Add Server", "HTTP"},
						Body:    l.text("connect.vscode.paste", name),
						Snippet: address,
					},
					{Title: l.text("connect.vscode.prompted"), Body: l.text("connect.vscode.signin", signIn)},
				},
			},
			{
				Slug: "codex",
				Name: "Codex",
				Steps: []connectStep{
					{Title: l.text("connect.codex.register"), Snippet: "codex mcp add " + name + " --url " + address},
					{
						Title:   l.text("connect.signin.title"),
						Body:    l.text("connect.codex.signin", signIn),
						Snippet: "codex mcp login " + name,
					},
				},
			},
			{
				Slug: "gemini-cli",
				Name: "Gemini CLI",
				Steps: []connectStep{
					{
						Title:   l.text("connect.gemini.add"),
						Body:    l.text("connect.gemini.merge"),
						Snippet: configBlock("mcpServers", name, map[string]any{"httpUrl": address}),
					},
					{Title: l.text("connect.gemini.restart"), Body: l.text("connect.gemini.signin", signIn)},
				},
			},
			{
				Slug: "windsurf",
				Name: "Windsurf",
				Steps: []connectStep{
					{
						Title:   l.text("connect.windsurf.add"),
						Body:    l.text("connect.windsurf.windows", `%USERPROFILE%`),
						Snippet: configBlock("mcpServers", name, map[string]any{"serverUrl": address}),
					},
					{Title: l.text("connect.windsurf.refresh"), Menu: []string{"Cascade", "MCP servers", "Refresh"}, Body: signIn},
				},
			},
			{
				Slug: "zed",
				Name: "Zed",
				// https://zed.dev/docs/ai/mcp
				Steps: []connectStep{
					{
						Title:   l.text("connect.zed.add"),
						Menu:    []string{"Zed", "Settings", "Open Settings"},
						Body:    l.text("connect.zed.merge"),
						Snippet: configBlock("context_servers", name, map[string]any{"url": address}),
					},
					{Title: l.text("connect.zed.check"), Menu: []string{"Settings", "AI", "MCP Servers"}, Body: l.text("connect.zed.signin", signIn)},
				},
			},
			{
				Slug: "manual",
				Name: l.text("connect.manual.name"),
				Steps: []connectStep{
					{
						Title:   l.text("connect.manual.add"),
						Body:    l.text("connect.manual.body"),
						Snippet: configBlock("mcpServers", name, map[string]any{"type": "http", "url": address}),
					},
					{Title: l.text("connect.manual.addr"), Snippet: address},
					{Title: l.text("connect.manual.authorize"), Body: l.text("connect.manual.signin", signIn)},
				},
			},
		},
	}, {
		Name: l.text("connect.group.vendor"),
		Hint: l.text("connect.group.vendor.hint"),
		Clients: []connectClient{
			{
				Slug: "claude",
				Name: "Claude",
				// https://support.claude.com/en/articles/11175166
				Caveat: l.text("connect.claude.caveat"),
				Vendor: "Anthropic",
				Steps: []connectStep{
					{
						Title:     l.text("connect.claude.open"),
						Menu:      []string{"Customize", "Connectors"},
						Body:      l.text("connect.claude.where"),
						Link:      "https://claude.ai/settings/connectors?modal=add-custom-connector",
						LinkLabel: l.text("connect.claude.open.link"),
					},
					{Title: l.text("connect.claude.choose"), Menu: []string{"+ Add", "Add custom connector"}, Body: l.text("connect.nameit", name)},
					{Title: l.text("connect.claude.paste"), Body: l.text("connect.claude.paste.body"), Snippet: address},
					{Title: l.text("connect.claude.agree"), Body: signIn},
				},
			},
			{
				Slug: "chatgpt",
				Name: "ChatGPT",
				// https://developers.openai.com/plugins/deploy/connect-chatgpt
				Caveat:     l.text("connect.chatgpt.caveat"),
				Vendor:     "OpenAI",
				NeedsHTTPS: true,
				Steps: []connectStep{
					{
						Title:     l.text("connect.chatgpt.open"),
						Menu:      []string{"+", "Add custom MCP server"},
						Body:      l.text("connect.nameit", name),
						Link:      "https://chatgpt.com/plugins",
						LinkLabel: l.text("connect.chatgpt.open.link"),
					},
					{Title: l.text("connect.chatgpt.paste"), Body: l.text("connect.chatgpt.paste.body"), Snippet: address},
					{Title: l.text("connect.chatgpt.create"), Body: l.text("connect.chatgpt.signin", signIn)},
				},
			},
		},
	}}

	tryIt := connectTryIt(l, name)
	for g := range groups {
		for c := range groups[g].Clients {
			groups[g].Clients[c].Steps = append(groups[g].Clients[c].Steps, tryIt)
		}

		groups[g].Fix = judgeReachIn(l, groups[g].Clients, address)
	}

	return groups
}

// judgeReach says, for each app whose vendor makes the connection, whether that vendor can reach address, and returns how to fix it when one cannot. A private hostname looks like a public one from here, so a public-looking address gets a warning rather than a pass.
func judgeReach(clients []connectClient, address string) string {
	return judgeReachIn(englishLocale(), clients, address)
}

func judgeReachIn(l *locale, clients []connectClient, address string) string {
	parsed, err := url.Parse(address)
	if err != nil {
		parsed = &url.URL{}
	}

	host := parsed.Hostname()
	private := privateHost(host)
	fix := ""

	for i := range clients {
		client := &clients[i]

		switch {
		case client.Vendor == "":
			continue
		case private:
			client.Blocked = l.text("connect.blocked.private", client.Name, client.Vendor, host)
		case client.NeedsHTTPS && parsed.Scheme != "https":
			client.Blocked = l.text("connect.blocked.https", client.Name)
		default:
			client.Reach = l.text("connect.reach", client.Vendor, host)

			continue
		}

		fix = l.text("connect.fix")
	}

	return fix
}

// privateHost is a host no vendor's servers could reach: this machine, or an address reserved for a private network.
func privateHost(host string) bool {
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified())
}
