package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/labstack/echo/v5"

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
// It exists because `--db shared.db` made "what does this file hold" a
// real question, and because the previous root answered a question nobody
// asked: it redirected to whichever slug sorted first, silently picking one
// of several.

// overviewLimit bounds the global feed. Smaller than historyLimit because
// this is the glance-at-it page: an operator who wants a pipeline's whole
// history opens that pipeline.
const overviewLimit = 50

// feedLength is how much of that feed the root draws until ?runs= asks for
// more: the newest few are what a glance is for.
const feedLength = 10

// feedLimit reads ?runs=, clamped to [feedLength, overviewLimit]; anything
// that is not a number is the default rather than an error page.
func feedLimit(asked string) int {
	n, err := strconv.Atoi(asked)
	if err != nil {
		return feedLength
	}

	return min(max(n, feedLength), overviewLimit)
}

// overviewRun is one row of the global feed: a run, plus the slug that makes
// it reachable.
type overviewRun struct {
	store.RunRow

	Pipeline string
}

// overviewPipeline is one served pipeline, with what the state file records
// about it alongside what this process knows.
//
// The last three fields are the answer to "is it moving, and if not, why" —
// the question this table could not answer at all. A paused pipeline's board
// says so on every one of its own pages, but the root is where an operator
// looks first, and there it read as a quiet one.
type overviewPipeline struct {
	Slug   string
	Jobs   int
	Paused bool
	// Latest is the pipeline's newest run across every job, zero when it has never run.
	Latest store.RunRow
	HasRun bool
	// Mark, Attention and InFlight are the switcher's own reading of the
	// pipeline, handed over rather than asked again, so the row and the
	// switcher cannot disagree. InFlight is what the trigger queue still owes
	// — the difference between a pipeline with nothing to do and one that is
	// not being drained.
	Mark      mark
	Attention int
	InFlight  int
	// Held is how many of its jobs the circuit breaker holds.
	Held int
	// Chips are the pipeline's jobs in its own order, each colored by its
	// latest run — the answer to "which job is red" that a last-run column
	// cannot give, and each the link to that job's transcript.
	Chips []jobChip
	// Broken is why the daemon holds this pipeline and does not serve it; every other field is zero then.
	Broken string
}

// overviewTally is the overview header's count of rows that want a person.
// Failing is read off the mark, so it agrees with the glyph on each row.
type overviewTally struct {
	Paused  int
	Failing int
	Broken  int
}

func tallyOverview(rows []overviewPipeline) overviewTally {
	var tally overviewTally

	for _, row := range rows {
		if row.Broken != "" {
			tally.Broken++

			continue
		}

		if row.Paused {
			tally.Paused++
		}

		if row.Mark.Disc >= discFailed {
			tally.Failing++
		}
	}

	return tally
}

// jobChip is one job on an overview row: its name and the st-* class the
// graph gives the same job.
type jobChip struct {
	Name  string
	Class string
}

// jobChips reads a pipeline's jobs through the board's own view of them, so
// the root and the graph cannot color one job two ways.
func jobChips(ctx context.Context, reads *pipelineReads) []jobChip {
	pipeline := reads.target

	latest, err := reads.LatestRunByJob(ctx)
	if err != nil {
		latest = nil
	}

	jobs := pipeline.Config().Jobs
	chips := make([]jobChip, 0, len(jobs))

	for _, job := range jobs {
		_, _, class := nodeStatus(buildJobView(job, latest, nil))
		chips = append(chips, jobChip{Name: job.Name, Class: class})
	}

	return chips
}

// handleIndex answers the bare root, and its answer depends on how many
// pipelines this process serves.
//
// One — the overwhelmingly common case — redirects straight through to that
// pipeline's board, so nobody pays a click for a list of one. Several render
// the overview, because with several there is no defensible pipeline to pick.
// None renders the empty page: a daemon holds nothing until somebody sets a
// pipeline into it, and the page that says how is the whole of its UI.
func (s *Server) handleIndex(c *echo.Context) error {
	served := s.Served()

	// A broken pipeline stops the shortcuts: redirected past, or shown the empty page, nobody would learn the daemon holds one it is not serving.
	broken := len(s.Broken()) > 0

	if len(served) == 0 && !broken {
		//nolint:wrapcheck // render errors surface through the shared error handler
		return c.Render(http.StatusOK, "empty", map[string]any{"Nav": s.globalNav(c)})
	}

	if len(served) == 1 && !broken {
		//nolint:wrapcheck // echo's redirect error is returned verbatim by every handler here
		return c.Redirect(http.StatusFound, "/p/"+served[0].Slug)
	}

	limit := feedLimit(c.QueryParam("runs"))

	// One past the limit, so "more" is drawn only when there is more.
	runs, err := s.recentRunsAcross(c.Request().Context(), limit+1)
	if err != nil {
		return err
	}

	more := len(runs) > limit && limit < overviewLimit
	runs = runs[:min(len(runs), limit)]

	// The shell gathered every served pipeline's attention already, to mark
	// the switcher; handing those counts to the table is what stops the root
	// asking the same six questions of the same databases twice per render.
	nav := s.globalNav(c)

	pipelines := s.overviewPipelines(c, nav)

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "overview", map[string]any{
		"Nav":       nav,
		"Title":     "pipelines",
		"Pipelines": pipelines,
		"Tally":     tallyOverview(pipelines),
		"Runs":      runs,
		"More":      more,
		"MoreURL":   fmt.Sprintf("/?runs=%d", overviewLimit),
		"MoreCount": overviewLimit,
	})
}

// overviewPipelines describes what this process serves, sorted by slug.
//
// Four reads per pipeline rather than one cross-pipeline query: a daemon
// holds a handful, they need not share a database, and a Reader can only
// answer what it can name. A read that fails leaves its field zero rather
// than failing the page — a root that will not render says less about a
// pipeline than a row missing one column.
func (s *Server) overviewPipelines(c *echo.Context, nav navData) []overviewPipeline {
	ctx := c.Request().Context()
	served := s.Served()

	summaries := make(map[string]pipelineSummary, len(nav.Pipelines))
	for _, summary := range nav.Pipelines {
		summaries[summary.Slug] = summary
	}

	out := make([]overviewPipeline, 0, len(served))

	for _, pipeline := range served {
		summary := summaries[pipeline.Slug]
		reads := readsOf(c, pipeline)
		row := overviewPipeline{
			Slug:      pipeline.Slug,
			Jobs:      len(pipeline.Config().Jobs),
			Paused:    reads.Paused(ctx),
			Mark:      summary.Mark,
			Attention: summary.Attention,
			InFlight:  summary.InFlight,
			Chips:     jobChips(ctx, reads),
		}

		runs, err := pipeline.Store.ListRuns(ctx, "", 1)
		if err == nil && len(runs) > 0 {
			row.Latest, row.HasRun = runs[0], true
		}

		held, err := reads.HeldJobs(ctx)
		if err == nil {
			row.Held = len(held)
		}

		out = append(out, row)
	}

	for _, broken := range s.Broken() {
		out = append(out, overviewPipeline{Slug: broken.Name, Broken: broken.Reason, Mark: summaries[broken.Name].Mark})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })

	return out
}

// recentRunsAcross reads the newest runs of every SERVED pipeline, newest
// first.
//
// Grouped by state FILE rather than queried per pipeline, because served
// pipelines need not share one: one daemon holding several pipelines gives each its
// own `.steps/<name>.db` unless --db says otherwise. Within a file, one
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
		handles = map[string]store.Store{}
		names   = map[string][]string{}
		// The store scopes itself by pipeline NAME and the URL is built from
		// the slug. They are the same string today — both come from
		// resolvePipelineName — but they are assigned in two places, and a
		// feed that assumed it would link every row somewhere wrong the day
		// that stopped being true.
		slugByName = map[string]string{}
	)

	for _, pipeline := range s.Served() {
		database := pipeline.Store.Description()
		name := pipeline.Store.Pipeline()

		handles[database] = pipeline.Store
		names[database] = append(names[database], name)
		slugByName[name] = pipeline.Slug
	}

	var runs []overviewRun

	for database, handle := range handles {
		rows, err := handle.Reader().RecentRuns(ctx, names[database], limit)
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
