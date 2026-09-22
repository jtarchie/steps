package web

// The mcp tab: what a pipeline's mcp_servers: are, who depends on each one, and whether it is actually wired up — plus the Connect button that finishes an oauth login in the browser already looking at this daemon. The browser half is SHORTER than `steps mcp login` rather than a second, harder path, because the whole problem that command solves is getting an authorization URL out of a daemon and into a browser that can reach that daemon: here, that problem is already solved.

import (
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/internal/config"
)

// mcpAuthorizeBound is how long Connect waits for the authorization URL before sending the reader back to the tab empty-handed. Discovery and dynamic registration are two round trips to the provider, so it is not instant; a provider that is simply down must not hold a request open either. The tab shows the pending login and links the URL the moment it lands, so the bound costs a reader nothing but a page they were going to see anyway.
const mcpAuthorizeBound = 10 * time.Second

// mcpAuthorizePoll is how often that wait re-reads the login's status. The status is in memory in this process; the cost of asking is a mutex.
const mcpAuthorizePoll = 25 * time.Millisecond

// MCPCredential is what the daemon knows about an oauth server's saved token without spending it. It carries no part of the token: a page renders this.
type MCPCredential struct {
	Connected bool
	// Detail is the sentence a reader acts on — "connected, renews automatically", "authorized for a different endpoint".
	Detail string
}

// MCPProbe is the outcome of a Test: a live connection to the server, made because somebody asked for one and never on a page load.
type MCPProbe struct {
	// Running is a probe still in flight, which the page's own poll will replace with its result.
	Running bool
	OK      bool
	Detail  string
	At      time.Time
	// Tools is what the server answered with, which is the question behind the question: a grant names a tool, and whether that name still exists is what a reader is really checking.
	Tools []string
}

// MCPState is everything the token-holder knows about one declared server. internal/web cannot import internal/mcp (depguard), and should not: a token file is not this package's business, and the interface is what keeps it that way.
type MCPState struct {
	Credential MCPCredential
	// Probe is the last Test of this server, nil when nobody has asked.
	Probe *MCPProbe
}

// mcpRow is one line of the table.
type mcpRow struct {
	Name   string
	UsedBy string
	// Target is the endpoint or argv in full, carried for the title attribute because Meta shows a URL without its scheme.
	Target string
	// OAuth marks the rows a login applies to, which is what puts a Connect button on one.
	OAuth bool
	// Status is the cell, and Mark is how it reads: the st-<mark> class the shared stylesheet draws the glyph and the colour from. THREE marks, not two, because a readiness nothing here answered — an oauth row on a daemon holding no token, a stdio cwd: that resolves per step — is not a pass, and `steps mcp list` prints it as neither.
	Status string
	Mark   string
	// Pending is a login in flight, with the authorization URL once there is one to offer.
	Pending      bool
	AuthorizeURL string
	// Failed is a login that got far enough to fail, which is the outcome the CLI polls for and the page must show too, since the exchange finishes after the browser has already come back.
	Failed string
	Probe  *MCPProbe
	// Meta is what the server IS, on one dimmed line: three columns of transport, target and auth were most of the page's width for facts a reader checks once. Nothing is dropped, only demoted.
	Meta string
	// CanTest is whether a connection could even be attempted. A server whose credential is already missing would answer a probe with the thing the status cell just said, in the words of whatever refused it — one problem in two wordings, and the longest string on the page. `steps mcp list` skips those for the same reason.
	CanTest bool
	// NeedsAttention is a server that cannot be used right now, or has a login in flight. It is what the page leads with, because the reader is triaging.
	NeedsAttention bool
}

// markFor is the stamp one static readiness reads as. MCPUnknown is st-skipped — faint, and not a tick — because the whole of that state is that nothing on this machine answered the question.
func markFor(readiness config.MCPReadiness) string {
	switch readiness {
	case config.MCPMissing:
		return "failed"
	case config.MCPReady:
		return "passed"
	case config.MCPUnknown:
		return "skipped"
	default:
		return "skipped"
	}
}

// mcpMetaLine is transport, credential and target on one line — everything the three columns before it said, in the order a reader asks it: what kind of server, what it authenticates with, and where it is. An auth of "none" is left out rather than printed, since "none" is the absence of a fact and reads as one.
func mcpMetaLine(server config.MCPServer) string {
	parts := []string{server.Transport()}

	if auth := server.AuthLabel(); auth != "none" {
		parts = append(parts, auth)
	}

	return strings.Join(append(parts, compactTarget(server)), " · ")
}

// compactTarget drops the scheme from an endpoint, which is the one part of a URL that is the same on every row and never what somebody is checking. The whole target stays on the element's title, so nothing is actually hidden.
func compactTarget(server config.MCPServer) string {
	target := server.Target()
	if server.IsStdio() {
		return target
	}

	for _, scheme := range []string{"https://", "http://"} {
		if after, found := strings.CutPrefix(target, scheme); found {
			return after
		}
	}

	return target
}

// mcpCtx is what the row template is invoked with: the page it is being drawn on, and the server to draw. The same shape stepCtx has, for the same reason — inside a {{define}}, $ is that template's own argument, so a sub-template needing the pipeline slug has to be handed it.
type mcpCtx struct {
	Page map[string]any
	Row  mcpRow
}

// handleMCP renders the tab. It makes no request to any declared server: a page that probed on load would connect to every one of them on every 2.5s poll of every open tab, which is a denial of service written against your own vendors.
func (s *Server) handleMCP(c *echo.Context) error {
	pipeline := pipelineOf(c)
	servers := pipeline.Config().MCPServers

	if len(servers) == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "this pipeline declares no mcp_servers:")
	}

	// Two groups, each keeping the file's own order inside it: what cannot be used leads, because that is the question somebody opened this page with, and a server declared tenth is no less broken for it.
	var attention, rest []mcpRow

	for _, server := range servers {
		row := s.mcpRowFor(pipeline, server)
		if row.NeedsAttention {
			attention = append(attention, row)

			continue
		}

		rest = append(rest, row)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "mcp", map[string]any{
		"Nav":       s.nav(c),
		"Attention": attention,
		"Rest":      rest,
		"Total":     len(servers),
		"Title":     "mcp",
	})
}

// mcpRowFor assembles one row from three sources that each know a different part: the configuration (what the server IS), this machine (a command on PATH, a variable set), and whoever holds the token.
func (s *Server) mcpRowFor(pipeline *Pipeline, server config.MCPServer) mcpRow {
	status := server.StaticStatus()

	row := mcpRow{
		Name:   server.Name,
		UsedBy: pipeline.Config().MCPUsers(server.Name),
		OAuth:  server.Auth.Type == "oauth",
		Status: status.Detail,
		Mark:   markFor(status.Readiness),
		Meta:   mcpMetaLine(server),
		Target: server.Target(),
	}

	authorizer := s.authorizer()
	if authorizer == nil {
		if row.OAuth {
			// Not "needs login": nothing here could run one, and telling a reader to click a button this build does not have is worse than saying so.
			row.Status = "this daemon cannot run an mcp login"
		}

		return row
	}

	state := authorizer.MCPState(pipeline, server.Name)
	row.Probe = state.Probe

	if row.OAuth {
		row.Status = state.Credential.Detail
		row.Mark = markFor(config.MCPMissing)

		if state.Credential.Connected {
			row.Mark = markFor(config.MCPReady)
		}
	}

	// A login in flight outranks the token it is about to replace: it is the newest thing that has happened to this server, and the reader is probably the one who started it.
	if login, found := authorizer.LoginStatus(server.Name); found && row.OAuth {
		switch login.State {
		case LoginPending:
			row.Pending, row.AuthorizeURL = true, login.AuthorizeURL
		case LoginFailed:
			row.Failed = login.Message
		}
	}

	row.settle()

	return row
}

// settle decides the two things the row's shape depends on, from everything else already on it.
func (r *mcpRow) settle() {
	// Only a server that could actually connect is worth dialling; the daemon refuses the rest anyway (probes.StartProbe), so offering the button would be offering a 400.
	r.CanTest = r.Mark == markFor(config.MCPReady)

	// A probe that failed is a working credential in front of a server that is not answering — which no static check can see, and which nobody would otherwise know to look for.
	probeFailed := r.Probe != nil && !r.Probe.Running && !r.Probe.OK

	// It also OUTRANKS the static answer, which is the older and weaker one: a
	// credential being present says a request could be made, and the probe is
	// the request. Leaving the tick up would put a green "no credential needed"
	// beside a red "did not answer" on one row, which is the same lie a
	// readiness nobody answered told when it rendered as a pass.
	if probeFailed {
		r.Mark, r.Status = markFor(config.MCPMissing), "did not answer"
	}

	r.NeedsAttention = r.Mark == markFor(config.MCPMissing) || r.Pending || r.Failed != ""
}

// handleMCPConnect starts a login and sends the browser to the provider — the whole of what `steps mcp login` does, minus the two halves that exist only because a terminal is not a browser: printing the authorization URL and polling for the result.
func (s *Server) handleMCPConnect(c *echo.Context) error {
	if s.runner == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server is read-only")
	}

	pipeline := pipelineOf(c)

	authorizer := s.authorizer()
	if authorizer == nil {
		return echo.NewHTTPError(http.StatusNotImplemented, "this daemon cannot run an mcp login")
	}

	base, err := browserOrigin(c)
	if err != nil {
		return err
	}

	server := c.Param("server")
	tab := "/p/" + pipeline.Slug + "/mcp"

	started, err := authorizer.StartLogin(pipeline, server, LoginRequest{Base: base, Return: tab})
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, awaitAuthorizeURL(c, authorizer, server, tab, started))
}

// awaitAuthorizeURL waits for discovery and registration to produce the URL the reader is to be sent to, and gives up on the tab rather than on an error page. Three ways out other than the bound: the URL arrives, which is the point; the login FAILS, since a provider that does not answer discovery should not cost a reader ten seconds of a spinner before it says so; and the login is REPLACED, because a login is tracked by server name and somebody else clicking Connect on that name takes the name over.
//
// The replacement case is why started is passed in. Asking for "the login called tracker" is the only question the interface can answer, and after a replacement that is somebody else's login — one that may be against a different endpoint entirely, since two pipelines may declare one name. Sending this reader to THAT consent screen asks them to authorize something they never clicked on; the tab, which shows the login that won, is the honest answer.
func awaitAuthorizeURL(c *echo.Context, authorizer Authorizer, server, tab string, started LoginStatus) string {
	deadline := time.Now().Add(mcpAuthorizeBound)

	for time.Now().Before(deadline) {
		status, found := authorizer.LoginStatus(server)
		if !found || status.State == LoginFailed || status.ID != started.ID {
			return tab
		}

		if status.AuthorizeURL != "" {
			return status.AuthorizeURL
		}

		select {
		case <-c.Request().Context().Done():
			return tab
		case <-time.After(mcpAuthorizePoll):
		}
	}

	return tab
}

// handleMCPTest probes one server and comes straight back. The probe is DETACHED because a preflight timeout is 30 seconds by default and a page is never otherwise slow: the click returns at once, the row says it is testing, and the poll that already drives this page swaps in the answer.
func (s *Server) handleMCPTest(c *echo.Context) error {
	if s.runner == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server is read-only")
	}

	pipeline := pipelineOf(c)

	authorizer := s.authorizer()
	if authorizer == nil {
		return echo.NewHTTPError(http.StatusNotImplemented, "this daemon cannot probe an mcp server")
	}

	err := authorizer.StartProbe(pipeline, c.Param("server"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, "/p/"+pipeline.Slug+"/mcp")
}

// browserOrigin is the address the READER reached this daemon on, which is what the redirect URI has to be built from and which the request itself cannot answer — behind a proxy the scheme is the proxy's, and a redirect URI registered as http:// for an https:// daemon is refused by the provider rather than by us. Origin is the browser's own answer, sameOriginMutations has already refused a POST whose Origin is not this host, and a request carrying none is not a browser at all — which is what /api and `steps mcp login` are for.
func browserOrigin(c *echo.Context) (string, error) {
	origin := c.Request().Header.Get("Origin")
	if origin == "" {
		return "", echo.NewHTTPError(http.StatusBadRequest,
			"a login started here needs the browser's Origin header; use `steps mcp login <server> -p <pipeline> --target <url>` from a terminal")
	}

	return strings.TrimSuffix(origin, "/"), nil
}
