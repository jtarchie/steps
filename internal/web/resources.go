package web

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/jtarchie/steps/internal/store"
	"github.com/labstack/echo/v5"
)

// handleResources shows what the watcher has seen, and the breaker state that
// stops it acting on what it sees.
func (s *Server) handleResources(c *echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	checked, err := pipeline.Store.CheckedResources(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	// The header's "N resources are failing their checks" links HERE, so this
	// is the page that owes the reader the reason. Without it the banner sends
	// somebody to a table of timestamps that looks fine.
	failing, err := pipeline.Store.CheckErrors(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "resources", map[string]any{
		"Nav":       s.nav(c),
		"Title":     "resources",
		"Resources": pipeline.Config().Resources,
		"Checked":   checkedByName(checked),
		"Failing":   failingByName(failing),
	})
}

// handleResource shows one resource's full recorded history — every version
// a check has reported, not just the latest, which is all handleResources
// has room for on the collection page.
func (s *Server) handleResource(c *echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()
	name := c.Param("resource")

	cfg := pipeline.Config()

	res, err := cfg.FindResource(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("no resource %q in this pipeline", name))
	}

	// The raw-JSON form, not decoded versions: jsonLine wants the same
	// string every other version field on this page already carries, and
	// this page never inspects a version's fields — decoding each one with
	// UseNumber only to re-encode it right back a moment later would be a
	// pointless round trip through this handler on every 2500ms self-poll.
	lines, err := pipeline.Store.ResourceVersionsJSON(ctx, res.Name)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	// Oldest-first (the order a check discovered them in); the newest
	// belongs at the top, matching the "Latest version" framing on the
	// collection page.
	slices.Reverse(lines)

	checked, _, err := pipeline.Store.LastChecked(ctx, res.Name)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	failing, err := pipeline.Store.CheckErrors(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	consumers := consumersOf(cfg, res.Name)

	rows, err := versionRows(ctx, pipeline.Store, res.Name, lines, consumers)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "resource", map[string]any{
		"Consumers": consumers,
		"Rows":      rows,
		"Span":      len(consumers) + 1,
		"Nav":       s.nav(c),
		"Title":     res.Name,
		"Crumbs":    []crumb{{Label: "resources", URL: "/p/" + pipeline.Slug + "/resources"}, {Label: res.Name}},
		"Resource":  res,
		"Checked":   checked,
		"Failing":   failingByName(failing)[res.Name],
	})
}

func checkedByName(rows []store.CheckedResource) map[string]store.CheckedResource {
	byName := map[string]store.CheckedResource{}
	for _, row := range rows {
		byName[row.Name] = row
	}

	return byName
}

// failingByName indexes check errors the way the templates ask for them: by
// resource, so a row can look up its own without walking the slice.
func failingByName(rows []store.CheckError) map[string]store.CheckError {
	byName := map[string]store.CheckError{}
	for _, row := range rows {
		byName[row.Name] = row
	}

	return byName
}
