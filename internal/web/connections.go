package web

// The agents a person has let connect to exports, and the way to disconnect
// one. A connection is a grant with a token still good (store.Connection);
// disconnecting ends every token under it, so the agent's next call fails
// and its next refresh is refused.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/internal/actor"
	"github.com/jtarchie/steps/internal/store"
)

const (
	connectionsPath = "/connections"
	revokePath      = "/connections/revoke"
	// callCountCap bounds how far a connection's calls are counted; past it the page says "N+".
	callCountCap = 1000
)

// connectionRow is one connection as the page draws it.
type connectionRow struct {
	store.Connection

	Hosts    []string
	LastCall store.ActivityEntry
	Calls    string
	Live     bool
}

// Reads is a connection to the read server rather than to one export.
func (r connectionRow) Reads() bool { return r.Kind == store.GrantRead }

// Display is a wall display rather than an agent: registration refuses an agent that names no redirect address, so a client with none is one this daemon made for a screen.
func (r connectionRow) Display() bool { return r.Kind == store.GrantRead && len(r.RedirectURIs) == 0 }

func (s *Server) handleConnections(c *echo.Context) error {
	return s.renderConnections(c, nil)
}

// renderConnections draws /connections, with extra beside what it always shows: the address of a display just added, which is shown that once.
func (s *Server) renderConnections(c *echo.Context, extra map[string]any) error {
	ctx := c.Request().Context()
	everyone := c.QueryParam("everyone") != "" && s.roleOfPerson(c) >= RoleAdmin

	filter := store.Grant{Actor: actor.From(ctx).String()}
	if everyone {
		filter = store.Grant{}
	}

	connections, err := s.sessions.Grants(ctx, filter)
	if err != nil {
		return echoError(err)
	}

	rows := make([]connectionRow, 0, len(connections))
	for _, conn := range connections {
		rows = append(rows, s.connectionRowFor(ctx, conn))
	}

	nav := s.globalNav(c)
	nav.Anchored = false

	values := map[string]any{
		"Nav":      nav,
		"Title":    s.renderer.localeOf(c).text("nav.connections"),
		"Rows":     rows,
		"Everyone": everyone,
		"CanAll":   s.roleOfPerson(c) >= RoleAdmin,
	}

	for key, value := range extra {
		values[key] = value
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "connections", values)
}

func (s *Server) connectionRowFor(ctx context.Context, conn store.Connection) connectionRow {
	row := connectionRow{Connection: conn, Calls: "0"}
	if row.Display() {
		return row
	}

	for _, uri := range conn.RedirectURIs {
		row.Hosts = append(row.Hosts, redirectHost(uri))
	}

	filter := store.ActivityFilter{Pipeline: conn.Pipeline, Actor: conn.Actor, Action: "mcp call", Target: conn.Export, Limit: callCountCap + 1}

	var reader store.Reader

	if conn.Kind == store.GrantRead {
		// Its calls are recorded against every pipeline they read, and the log is one per database, so any reader of it counts them.
		row.Live = true
		filter = store.ActivityFilter{Actor: conn.Actor, Action: readAction, Limit: callCountCap + 1}

		var release func()

		reader, release = s.activityReader()
		defer release()
	} else if target := s.Lookup(conn.Pipeline); target != nil {
		row.Live = true
		reader = target.Store.Reader()
	}

	if reader == nil {
		return row
	}

	calls, err := reader.Activity(ctx, filter)
	if err != nil || len(calls) == 0 {
		return row
	}

	row.LastCall = calls[0]
	row.Calls = strconv.Itoa(min(len(calls), callCountCap))

	if len(calls) > callCountCap {
		row.Calls += "+"
	}

	return row
}

// handleRevoke disconnects one agent. A person disconnects their own; an admin may disconnect anybody's, which is what the everyone view is for.
func (s *Server) handleRevoke(c *echo.Context) error {
	ctx := c.Request().Context()
	me := actor.From(ctx).String()

	grant := store.Grant{Kind: store.GrantExport, ClientID: c.FormValue("client"), Actor: c.FormValue("actor"), Pipeline: c.FormValue("pipeline"), Export: c.FormValue("export")}
	if c.FormValue("kind") == string(store.GrantRead) {
		grant = store.Grant{Kind: store.GrantRead, ClientID: grant.ClientID, Actor: grant.Actor}
	}

	if grant.Actor == "" {
		grant.Actor = me
	}

	if grant.Actor != me && s.roleOfPerson(c) < RoleAdmin {
		return echo.NewHTTPError(http.StatusForbidden, "only an admin can disconnect somebody else's agent")
	}

	err := s.sessions.RevokeGrant(context.WithoutCancel(ctx), grant)
	if err != nil {
		return echoError(err)
	}

	if grant.Kind == store.GrantRead {
		slog.InfoContext(ctx, "web.mcp_read_disconnected", "actor", grant.Actor, "by", me, "client", grant.ClientID)
	} else if target := s.Lookup(grant.Pipeline); target != nil {
		detail, err := json.Marshal(map[string]string{"by": me, "client": grant.ClientID})
		if err == nil {
			s.recordGrantActivity(ctx, target, store.Grant{Kind: store.GrantExport, ClientID: grant.ClientID, Actor: me, Pipeline: grant.Pipeline, Export: grant.Export}, "mcp disconnect", string(detail))
		}
	}

	back := connectionsPath
	if c.FormValue("everyone") != "" {
		back += "?" + url.Values{"everyone": {"1"}}.Encode()
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, back)
}
