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
func connectTryIt(name string) connectStep {
	return connectStep{Title: "Ask it something", Body: "If an answer comes back, it is working.", Snippet: "Using " + name + ", list the tools you have."}
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
	signIn := "Sign in to steps if it asks, then press Allow on the page that names " + name + "."

	groups := []connectGroup{{
		Name: "From your machine",
		Hint: "terminal or editor",
		Clients: []connectClient{
			{
				Slug: "claude-code",
				Name: "Claude Code",
				Steps: []connectStep{
					{
						Title:   "Run this in your terminal",
						Body:    "--scope user registers it once for every project; without it the server exists only in the folder you ran it in.",
						Snippet: "claude mcp add --transport http --scope user " + name + " " + address,
					},
					{
						Title: "Open the MCP list and authenticate",
						Body:  "Type /mcp in Claude Code, choose " + name + ", then Authenticate.",
					},
					{Title: "Agree in the browser", Body: signIn},
				},
			},
			{
				Slug: "cursor",
				Name: "Cursor",
				Steps: []connectStep{
					{
						Title:     "Install it with one click",
						Body:      "Cursor opens and asks to confirm.",
						Link:      cursorDeepLink(name, address),
						LinkLabel: "Add " + name + " to Cursor",
					},
					{
						Title:   "Or add it by hand",
						Body:    "If the button did nothing, put this in ~/.cursor/mcp.json instead.",
						Snippet: configBlock("mcpServers", name, map[string]any{"type": "http", "url": address}),
					},
					{
						Title: "Sign in",
						Menu:  []string{"Settings", "MCP"},
						Body:  name + " shows Needs login — click it. " + signIn,
					},
				},
			},
			{
				Slug: "vscode",
				Name: "VS Code",
				Steps: []connectStep{
					{
						Title:     "Install it with one click",
						Body:      "On VS Code Insiders, use the by-hand path below instead.",
						Link:      vscodeDeepLink(name, address),
						LinkLabel: "Add " + name + " to VS Code",
					},
					{
						Title:   "Or add it by hand",
						Menu:    []string{"Command Palette", "MCP: Add Server", "HTTP"},
						Body:    "Paste this address when it asks for the URL, and name it " + name + ".",
						Snippet: address,
					},
					{Title: "Sign in when prompted", Body: "VS Code asks permission to authenticate, then opens the browser. " + signIn},
				},
			},
			{
				Slug: "codex",
				Name: "Codex",
				Steps: []connectStep{
					{Title: "Register the server", Snippet: "codex mcp add " + name + " --url " + address},
					{
						Title:   "Sign in",
						Body:    "This opens a browser window and keeps the token for you. " + signIn,
						Snippet: "codex mcp login " + name,
					},
				},
			},
			{
				Slug: "gemini-cli",
				Name: "Gemini CLI",
				Steps: []connectStep{
					{
						Title:   "Add this to ~/.gemini/settings.json",
						Body:    "Merge it into the mcpServers block if you already have one.",
						Snippet: configBlock("mcpServers", name, map[string]any{"httpUrl": address}),
					},
					{Title: "Restart Gemini CLI and sign in", Body: "It finds the sign-in on its own and opens a browser the first time it connects. " + signIn},
				},
			},
			{
				Slug: "windsurf",
				Name: "Windsurf",
				Steps: []connectStep{
					{
						Title:   "Add this to ~/.codeium/windsurf/mcp_config.json",
						Body:    `On Windows the file is under %USERPROFILE%\.codeium\windsurf\.`,
						Snippet: configBlock("mcpServers", name, map[string]any{"serverUrl": address}),
					},
					{Title: "Refresh, then sign in", Menu: []string{"Cascade", "MCP servers", "Refresh"}, Body: signIn},
				},
			},
			{
				Slug: "zed",
				Name: "Zed",
				// https://zed.dev/docs/ai/mcp
				Steps: []connectStep{
					{
						Title:   "Add this to Zed's settings.json",
						Menu:    []string{"Zed", "Settings", "Open Settings"},
						Body:    "Merge it into the context_servers block if you already have one. Leave out any headers: without them, Zed signs in with OAuth.",
						Snippet: configBlock("context_servers", name, map[string]any{"url": address}),
					},
					{Title: "Check that it started, then sign in", Menu: []string{"Settings", "AI", "MCP Servers"}, Body: "Zed asks you to authenticate the first time it connects. " + signIn},
				},
			},
			{
				Slug: "manual",
				Name: "Anything else",
				Steps: []connectStep{
					{
						Title:   "Add this entry to your app's MCP config",
						Body:    "Most apps read a block like this one — a project's .mcp.json, for one. Restart the app afterwards.",
						Snippet: configBlock("mcpServers", name, map[string]any{"type": "http", "url": address}),
					},
					{Title: "Or, if it only asks for an address", Snippet: address},
					{Title: "Authorize it", Body: "steps speaks OAuth and registers apps on its own; there is no key or token to paste. " + signIn},
				},
			},
		},
	}, {
		Name: "From the vendor's servers",
		Hint: "chat apps",
		Clients: []connectClient{
			{
				Slug: "claude",
				Name: "Claude",
				// https://support.claude.com/en/articles/11175166
				Caveat: "A Free plan allows one custom connector. On Team and Enterprise, an owner adds it under Organization settings → Connectors.",
				Vendor: "Anthropic",
				Steps: []connectStep{
					{
						Title:     "Open Claude's connector settings",
						Menu:      []string{"Customize", "Connectors"},
						Body:      "In the Claude app, or on claude.ai in a browser.",
						Link:      "https://claude.ai/settings/connectors?modal=add-custom-connector",
						LinkLabel: "Open Claude connector settings",
					},
					{Title: "Choose to add a custom connector", Menu: []string{"+ Add", "Add custom connector"}, Body: "Name it " + name + "."},
					{Title: "Paste this address into the URL box", Body: "Then press Add. Leave the advanced settings alone — there is no key to enter.", Snippet: address},
					{Title: "Agree when the window opens", Body: signIn},
				},
			},
			{
				Slug: "chatgpt",
				Name: "ChatGPT",
				// https://developers.openai.com/plugins/deploy/connect-chatgpt
				Caveat:     "Account and workspace policies decide who may add a custom MCP server; on a work account an admin may have to allow it first.",
				Vendor:     "OpenAI",
				NeedsHTTPS: true,
				Steps: []connectStep{
					{
						Title:     "Open ChatGPT's plugins",
						Menu:      []string{"+", "Add custom MCP server"},
						Body:      "Name it " + name + ".",
						Link:      "https://chatgpt.com/plugins",
						LinkLabel: "Open ChatGPT plugins",
					},
					{Title: "Paste this address under Connection", Body: "Choose the endpoint URL, not Tunnel. For authentication, choose OAuth.", Snippet: address},
					{Title: "Create it, then agree when it asks", Body: "ChatGPT registers itself on its own. " + signIn},
				},
			},
		},
	}}

	tryIt := connectTryIt(name)
	for g := range groups {
		for c := range groups[g].Clients {
			groups[g].Clients[c].Steps = append(groups[g].Clients[c].Steps, tryIt)
		}

		groups[g].Fix = judgeReach(groups[g].Clients, address)
	}

	return groups
}

// judgeReach says, for each app whose vendor makes the connection, whether that vendor can reach address, and returns how to fix it when one cannot. A private hostname looks like a public one from here, so a public-looking address gets a warning rather than a pass.
func judgeReach(clients []connectClient, address string) string {
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
			client.Blocked = client.Name + " connects from " + client.Vendor + "'s servers, which cannot reach " + host + "."
		case client.NeedsHTTPS && parsed.Scheme != "https":
			client.Blocked = client.Name + " only connects to an https address."
		default:
			client.Reach = client.Vendor + "'s servers make this connection, not your machine, so " + host + " must be reachable from the internet."

			continue
		}

		fix = "To offer these, start steps web with --external-url set to an https address reachable from the internet."
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
