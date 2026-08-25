package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/labstack/echo/v4"

	"github.com/jtarchie/steps/internal/store"
)

// The root, when this process serves more than one pipeline.
//
// Everything else in this package is scoped to a pipeline by a route
// parameter, and deliberately: `/p/<slug>/...` is the whole UI, and a
// `Pipeline` handle is what a handler reads through. The overview is the one
// view above that, so it is the one place that reads through store.Reader —
// which crosses pipelines by construction and therefore has to name them.
//
// It exists because `--state shared.db` made "what does this file hold" a
// real question, and because the previous root answered a question nobody
// asked: it redirected to whichever slug sorted first, silently picking one
// of several.

// overviewLimit bounds the global feed. Smaller than historyLimit because
// this is the glance-at-it page: an operator who wants a pipeline's whole
// history opens that pipeline.
const overviewLimit = 50

// overviewRun is one row of the global feed: a run, plus the slug that makes
// it reachable.
type overviewRun struct {
	store.RunRow

	Pipeline string
}

// overviewPipeline is one served pipeline, with what the state file records
// about it alongside what this process knows.
type overviewPipeline struct {
	Slug string
	Path string
	Jobs int
}

// handleIndex answers the bare root, and its answer depends on how many
// pipelines this process serves.
//
// One — the overwhelmingly common case — redirects straight through to that
// pipeline's board, so nobody pays a click for a list of one. Several render
// the overview, because with several there is no defensible pipeline to pick.
func (s *Server) handleIndex(c echo.Context) error {
	if len(s.pipelines) == 1 {
		//nolint:wrapcheck // echo's redirect error is returned verbatim by every handler here
		return c.Redirect(http.StatusFound, "/p/"+s.pipelines[0].Slug)
	}

	runs, err := s.recentRunsAcross(c.Request().Context(), overviewLimit)
	if err != nil {
		return err
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "overview", map[string]any{
		"Nav":       s.nav(c),
		"Pipelines": s.overviewPipelines(),
		"Runs":      runs,
	})
}

// overviewPipelines describes what this process serves, sorted by slug.
func (s *Server) overviewPipelines() []overviewPipeline {
	out := make([]overviewPipeline, 0, len(s.pipelines))
	for _, pipeline := range s.pipelines {
		out = append(out, overviewPipeline{
			Slug: pipeline.Slug,
			Path: pipeline.Path,
			Jobs: len(pipeline.Cfg.Jobs),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })

	return out
}

// recentRunsAcross reads the newest runs of every SERVED pipeline, newest
// first.
//
// Grouped by state FILE rather than queried per pipeline, because served
// pipelines need not share one: `steps web app.yml infra.yml` gives each its
// own `.steps/<name>.db` unless --state says otherwise. Within a file, one
// ordered query does the interleaving; across files there is nothing to do
// but merge, and each group returns its own top `limit` so the merge cannot
// be short of rows it should have had.
//
// Only pipelines this process serves are named, so a file shared with a
// pipeline nobody here loaded contributes nothing — a row with no route
// behind it is worse than a missing one, and the feed is bounded, so it would
// crowd out rows that do have one.
func (s *Server) recentRunsAcross(ctx context.Context, limit int) ([]overviewRun, error) {
	var (
		handles = map[string]*store.Store{}
		names   = map[string][]string{}
		// The store scopes itself by pipeline NAME and the URL is built from
		// the slug. They are the same string today — both come from
		// resolvePipelineName — but they are assigned in two places, and a
		// feed that assumed it would link every row somewhere wrong the day
		// that stopped being true.
		slugByName = map[string]string{}
	)

	for _, pipeline := range s.pipelines {
		path := pipeline.Store.Path()
		name := pipeline.Store.Pipeline()

		handles[path] = pipeline.Store
		names[path] = append(names[path], name)
		slugByName[name] = pipeline.Slug
	}

	var runs []overviewRun

	for path, handle := range handles {
		rows, err := handle.Reader().RecentRuns(ctx, names[path], limit)
		if err != nil {
			return nil, fmt.Errorf("web: %w", err)
		}

		for _, row := range rows {
			runs = append(runs, overviewRun{RunRow: row.RunRow, Pipeline: slugByName[row.Pipeline]})
		}
	}

	sort.SliceStable(runs, func(i, j int) bool {
		return runs[i].StartedAt.After(runs[j].StartedAt)
	})

	if len(runs) > limit {
		runs = runs[:limit]
	}

	return runs, nil
}
