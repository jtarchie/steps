package web

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/jtarchie/steps/internal/config"
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

	loc := s.renderer.localeOf(c)
	cfg := pipeline.Config()
	list := newList("/p/"+pipeline.Slug+"/resources", c.Request().URL.Query(), loc.text("resources.search.hint"), loc.text("resources.search.label"))

	var types []string

	for _, res := range cfg.Resources {
		if !slices.Contains(types, res.Type) {
			types = append(types, res.Type)
		}
	}

	state := list.facet(loc, "status", resourceStates, func(v string) string { return loc.word("resstate", v) })
	kind := list.facet(loc, "type", types, identity)
	list.searched()

	rows := resourceRows(cfg, checkedByName(checked), failingByName(failing))
	total := len(rows)

	rows = slices.DeleteFunc(rows, func(row resourceRow) bool {
		return (state != "" && row.State != state) || (kind != "" && row.Type != kind) ||
			!matches(list.Text, row.Name, row.Type, string(row.Checked.Version))
	})
	list.Shown, list.Total = len(rows), total

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "resources", map[string]any{
		"Nav":   s.nav(c),
		"Title": "resources",
		"Rows":  rows,
		"Total": total,
		"List":  list,
		// Only where a poller can take the request: a server nothing polls for draws no button that could only refuse.
		"CanCheck": pipeline.CheckNow != nil,
	})
}

// resourceStates are the status chips' values, in the order a reader triages them.
var resourceStates = []string{"failing", "unchecked", "ok"}

// resourceRow is one resource as the list draws it.
type resourceRow struct {
	Name, Type string
	// State is failing, unchecked (no check ever succeeded) or ok.
	State   string
	Checked store.CheckedResource
	Failing store.CheckError
	// UsedBy is every job that gets or puts it, so a reader can go from what changed to what it moves.
	UsedBy []string
}

func resourceRows(cfg *config.Config, checked map[string]store.CheckedResource, failing map[string]store.CheckError) []resourceRow {
	rows := make([]resourceRow, 0, len(cfg.Resources))

	for _, res := range cfg.Resources {
		row := resourceRow{Name: res.Name, Type: res.Type, Checked: checked[res.Name], Failing: failing[res.Name], State: "ok"}

		switch {
		case row.Failing.Name != "":
			row.State = "failing"
		case row.Checked.Name == "":
			row.State = "unchecked"
		}

		for _, job := range cfg.Jobs {
			if slices.Contains(job.ResourceNames(), res.Name) {
				row.UsedBy = append(row.UsedBy, job.Name)
			}
		}

		rows = append(rows, row)
	}

	return rows
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

	loc := s.renderer.localeOf(c)
	list := newList("/p/"+pipeline.Slug+"/resources/"+res.Name, c.Request().URL.Query(), loc.text("resource.search.hint"), loc.text("resource.search.label"))
	list.searched()

	// Before the rows are built, which costs a query per version: a narrowed page reads only what it shows.
	total := len(lines)
	lines = slices.DeleteFunc(lines, func(line store.VersionKey) bool { return !matches(list.Text, string(line)) })
	list.Shown, list.Total = len(lines), total

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
		"Crumbs":    []crumb{{Label: s.renderer.localeOf(c).text("tab.resources"), URL: "/p/" + pipeline.Slug + "/resources"}, {Label: res.Name}},
		"Resource":  res,
		"Checked":   checked,
		"Failing":   failingByName(failing)[res.Name],
		"List":      list,
		"Total":     total,
		"CanCheck":  pipeline.CheckNow != nil,
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

// handleCheck asks for one resource to be checked now, as Concourse's check button does, through the pipeline's own poller.
func (s *Server) handleCheck(c *echo.Context) error {
	err := s.requireControls()
	if err != nil {
		return err
	}

	pipeline := pipelineOf(c)
	ctx := c.Request().Context()
	name := c.Param("resource")

	_, err = pipeline.Config().FindResource(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("no resource %q in this pipeline", name))
	}

	if pipeline.CheckNow == nil {
		return echo.NewHTTPError(http.StatusConflict, "nothing checks resources here: steps web is what polls")
	}

	stopped, err := pipeline.Store.Paused(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	if stopped {
		return pausedRefusal(ctx, pipeline)
	}

	if !pipeline.CheckNow(name) {
		return echo.NewHTTPError(http.StatusTooManyRequests, "checks are already waiting; try again once they have run")
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, returnTo(c, "/p/"+pipeline.Slug+"/resources/"+name))
}
