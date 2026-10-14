package web

// What a pipeline shares: the exports section of its mcp tab, and a page per
// export with its address, how to connect, who may, what it offers and what
// has been called. Read-only throughout — the pipeline file is where an export
// is changed.

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// exportRow is one export as the mcp tab lists it.
type exportRow struct {
	config.MCPExport

	People     int
	Agents     int
	CallsToday int
}

func (s *Server) exportRows(c *echo.Context, pipeline *Pipeline) []exportRow {
	exports := pipeline.Config().MCPExports
	rows := make([]exportRow, 0, len(exports))

	for _, export := range exports {
		rows = append(rows, s.exportRowFor(c.Request().Context(), pipeline, export))
	}

	return rows
}

func (s *Server) exportRowFor(ctx context.Context, pipeline *Pipeline, export config.MCPExport) exportRow {
	row := exportRow{MCPExport: export}

	if s.sessions != nil {
		connections, err := s.sessions.Grants(ctx, store.Grant{Pipeline: pipeline.Slug, Export: export.Name})
		if err == nil {
			row.Agents = len(connections)
			row.People = distinctActors(connections)
		}
	}

	calls, err := pipeline.Store.Reader().Activity(ctx, store.ActivityFilter{Pipeline: pipeline.Slug, Action: "mcp call", Target: export.Name, Since: startOfDay(time.Now())})
	if err == nil {
		row.CallsToday = len(calls)
	}

	return row
}

func distinctActors(connections []store.Connection) int {
	actors := make([]string, 0, len(connections))
	for _, conn := range connections {
		if !slices.Contains(actors, conn.Actor) {
			actors = append(actors, conn.Actor)
		}
	}

	return len(actors)
}

func startOfDay(now time.Time) time.Time {
	year, month, day := now.Date()

	return time.Date(year, month, day, 0, 0, 0, 0, now.Location())
}

// exportActions are the verbs whose target is an export's name; a job or a server may share that name and is a different thing.
var exportActions = []string{"mcp call", "mcp authorize", "mcp disconnect"} //nolint:gochecknoglobals // a fixed vocabulary

// recentExportCalls is how many of an export's calls its page shows before linking the whole log.
const recentExportCalls = 10

func (s *Server) handleExportPage(c *echo.Context) error {
	pipeline := pipelineOf(c)

	var (
		export config.MCPExport
		found  bool
	)

	for _, candidate := range pipeline.Config().MCPExports {
		if candidate.Name == c.Param("export") {
			export, found = candidate, true
		}
	}

	if !found {
		return echo.NewHTTPError(http.StatusNotFound, "this pipeline has no export by that name")
	}

	ctx := c.Request().Context()
	row := s.exportRowFor(ctx, pipeline, export)

	recent, err := pipeline.Store.Reader().Activity(ctx, store.ActivityFilter{Pipeline: pipeline.Slug, Actions: exportActions, Target: export.Name, Limit: recentExportCalls})
	if err != nil {
		return echoError(err)
	}

	rows := make([]activityRow, 0, len(recent))
	for _, entry := range recent {
		rows = append(rows, s.activityRowFor(entry))
	}

	nav := s.nav(c)
	nav.Anchored = true

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "export", map[string]any{
		"Nav":     nav,
		"Title":   export.Name + " · mcp",
		"Crumbs":  []crumb{{Label: "mcp", URL: "/p/" + pipeline.Slug + "/mcp"}, {Label: export.Name}},
		"Export":  row,
		"Address": s.resourceURL(pipeline.Slug, export.Name),
		"Serving": s.exporter != nil,
		"Recent":  rows,
		"LogURL":  "/activity?" + url.Values{"pipeline": {pipeline.Slug}, "target": {export.Name}}.Encode(),
	})
}
