package web

// The agents a person has let connect to exports, and the way to disconnect
// one. A connection is a grant with a token still good (store.Connection);
// disconnecting ends every token under it, so the agent's next call fails
// and its next refresh is refused.

import (
	"context"
	"encoding/json"
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

func (s *Server) handleConnections(c *echo.Context) error {
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

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "connections", map[string]any{
		"Nav":      nav,
		"Title":    "connections",
		"Rows":     rows,
		"Everyone": everyone,
		"CanAll":   s.roleOfPerson(c) >= RoleAdmin,
	})
}

func (s *Server) connectionRowFor(ctx context.Context, conn store.Connection) connectionRow {
	row := connectionRow{Connection: conn, Calls: "0"}

	for _, uri := range conn.RedirectURIs {
		row.Hosts = append(row.Hosts, redirectHost(uri))
	}

	target := s.Lookup(conn.Pipeline)
	row.Live = target != nil

	if target == nil {
		return row
	}

	calls, err := target.Store.Reader().Activity(ctx, store.ActivityFilter{Pipeline: conn.Pipeline, Actor: conn.Actor, Action: "mcp call", Target: conn.Export, Limit: callCountCap + 1})
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

	grant := store.Grant{ClientID: c.FormValue("client"), Actor: c.FormValue("actor"), Pipeline: c.FormValue("pipeline"), Export: c.FormValue("export")}
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

	if target := s.Lookup(grant.Pipeline); target != nil {
		detail, err := json.Marshal(map[string]string{"by": me, "client": grant.ClientID})
		if err == nil {
			s.recordGrantActivity(ctx, target, store.Grant{ClientID: grant.ClientID, Actor: me, Pipeline: grant.Pipeline, Export: grant.Export}, "mcp disconnect", string(detail))
		}
	}

	back := connectionsPath
	if c.FormValue("everyone") != "" {
		back += "?" + url.Values{"everyone": {"1"}}.Encode()
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, back)
}
