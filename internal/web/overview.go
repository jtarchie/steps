package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/internal/config"
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
	// Latest is the run that made the row red, else its newest run across every job; zero when it has never run.
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
	Next   nextView
}

// nextView is what makes the pipeline move next without a person: the earliest cron slot, and the other resources whose new versions trigger it.
type nextView struct {
	At time.Time
	On []string
}

// stripJobs is the widest pipeline whose every job gets a run strip; past it a strip per job wraps the row, so only red jobs keep theirs.
const stripJobs = 4

// laneRuns is how many of a job's runs its strip shows.
const laneRuns = 10

// Red is a row that wants a person: a job failed, the breaker holds one, or
// the daemon cannot serve it. Read off the mark, so the stripe agrees with the
// glyph beside it; a paused pipeline that is also failing is red, as its mark is.
func (p overviewPipeline) Red() bool {
	return p.Broken != "" || p.Held > 0 || p.Mark.Disc == discFailed
}

// RedWords is what the root's live region says of a red row: the jobs that
// made it red, or that it is not being served at all.
func (p overviewPipeline) RedWords() string {
	if p.Broken != "" {
		return "not being served"
	}

	words := []string{}

	for _, chip := range p.Chips {
		if chip.Red() {
			words = append(words, chip.Name+" "+chip.Word)
		}
	}

	return strings.Join(words, ", ")
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

// jobChip is one job on an overview row: a cell colored the way the graph
// colors the same node — by its latest FINISHED run, so a red job being
// rebuilt stays red with a running bar under it, the rule the pipeline's
// mark folds by.
// Held outranks the run: the breaker is why it will not move on its own.
type jobChip struct {
	Name  string
	Class string
	// Glyph is the cell's whole visible content; Word is what the cell says
	// to a reader who cannot see it.
	Glyph   string
	Word    string
	Running bool
	Held    bool
	// Fed is a job that gets a resource passed: by the job drawn before it, which is what earns the arrow between them.
	Fed bool
	// Run is the finished run the cell is colored by, for Latest to follow on a red row.
	Run    store.RunRow
	HasRun bool
	// History is the job's last laneRuns runs, oldest first.
	History []store.RunRow
}

// Red is a chip whose run Latest reports and whose strip survives the wide-pipeline cap.
func (c jobChip) Red() bool {
	return c.Held || c.Class == "st-failed" || c.Class == "st-errored"
}

// jobChips reads a pipeline's jobs through the board's own view of them, so the root and the graph cannot color one job two ways. Every read here but the strips' is one the pipeline's mark already made, memoized on reads.
func jobChips(ctx context.Context, reads *pipelineReads) []jobChip {
	pipeline := reads.target

	latest, err := reads.LatestRunByJob(ctx)
	if err != nil {
		latest = nil
	}

	// A read that fails leaves every cell unheld rather than failing the row.
	held, err := reads.HeldJobs(ctx)
	if err != nil {
		held = nil
	}

	heldBy := map[string]store.HeldJob{}
	for _, job := range held {
		heldBy[job.Name] = job
	}

	jobs := pipeline.Config().Jobs
	views := make([]jobView, 0, len(jobs))

	for _, job := range jobs {
		views = append(views, buildJobView(job, latest, heldBy))
	}

	withLastFinished(ctx, reads, views)

	chips := make([]jobChip, 0, len(views))

	for i, view := range views {
		chip := chipFor(view)
		chip.Fed = i > 0 && fedBy(view, views[i-1].Name)

		if view.HasRun && (len(views) <= stripJobs || chip.Red()) {
			chip.History = jobHistory(ctx, pipeline, view.Name)
		}

		chips = append(chips, chip)
	}

	return chips
}

func chipFor(view jobView) jobChip {
	chip := jobChip{
		Name:    view.Name,
		Class:   finishedClass(view),
		Running: view.HasRun && statusWord(view.Latest.Status) == "running",
		Held:    view.Held,
	}

	chip.Run, chip.HasRun = lastFinished(view)

	chip.Word = strings.TrimPrefix(chip.Class, "st-")
	if chip.Word == "none" {
		chip.Word = "never ran"
	}

	chip.Glyph = statusMark(chip.Word)

	if chip.Held {
		chip.Class, chip.Glyph, chip.Word = "st-held", "⊘", "held"
	}

	return chip
}

func fedBy(view jobView, upstream string) bool {
	for _, input := range view.Inputs {
		if slices.Contains(input.Passed, upstream) {
			return true
		}
	}

	return false
}

// jobHistory is one read per job per poll, the job page's own ListRuns; a read that fails draws no strip rather than failing the row.
// ponytail: per-job reads; a grouped "last N per job" store query if a daemon's poll ever shows up in a profile.
func jobHistory(ctx context.Context, pipeline *Pipeline, job string) []store.RunRow {
	runs, err := pipeline.Store.ListRuns(ctx, job, laneRuns)
	if err != nil {
		return nil
	}

	slices.Reverse(runs)

	return runs
}

// redRun is the run that made a red row red, so Latest cannot report a later green run of another job beside a red mark. With several red jobs it is the newest, not the first in config order: a days-old failure would otherwise stand as Latest beside a fresh one.
func redRun(chips []jobChip) (store.RunRow, bool) {
	var (
		newest store.RunRow
		found  bool
	)

	for _, chip := range chips {
		if chip.Red() && chip.HasRun && (!found || chip.Run.StartedAt.After(newest.StartedAt)) {
			newest, found = chip.Run, true
		}
	}

	return newest, found
}

// nextFor reads the pipeline's triggers off its config: cron resources become a time through cronNext, the rest are named. A cron that cannot answer is named instead, so the cell never claims manual for a triggered pipeline. A job the breaker holds is skipped: its automatic triggers are dropped at dispatch, so only a person moves it.
func nextFor(cfg *config.Config, held map[string]bool, cronNext CronNext, now time.Time) nextView {
	var (
		next nextView
		seen []string
	)

	for _, job := range cfg.Jobs {
		if held[job.Name] {
			continue
		}

		for _, input := range job.Inputs() {
			// A passed: input is handed over by an upstream job, not something the pipeline waits on from outside.
			if !input.Trigger || len(input.Passed) > 0 || slices.Contains(seen, input.Resource) {
				continue
			}

			seen = append(seen, input.Resource)

			at, ok := cronAt(cfg, input.Resource, cronNext, now)
			if !ok {
				next.On = append(next.On, input.Resource)

				continue
			}

			if next.At.IsZero() || at.Before(next.At) {
				next.At = at
			}
		}
	}

	return next
}

func cronAt(cfg *config.Config, name string, cronNext CronNext, now time.Time) (time.Time, bool) {
	res, err := cfg.FindResource(name)
	if err != nil || res.Type != config.CronType || cronNext == nil {
		return time.Time{}, false
	}

	at, err := cronNext(*res, now)

	return at, err == nil && !at.IsZero()
}

// nextLayout coarsens a fire time with distance: the clock later today, the weekday within a week, the date past that.
func nextLayout(now, at time.Time) string {
	now = now.In(at.Location())

	switch {
	case at.Sub(now) < 24*time.Hour && at.YearDay() == now.YearDay():
		return "15:04"
	// Same weekday as today but not today is next week's: "Wed 07:00" read on a Wednesday says this morning.
	case at.Sub(now) < 7*24*time.Hour && at.Weekday() != now.Weekday():
		return "Mon 15:04"
	default:
		return "Jan 2"
	}
}

func nextTag(at time.Time) template.HTML {
	//nolint:gosec // G203: every interpolation is machine-formatted, not input
	return template.HTML(fmt.Sprintf(`<time datetime=%q title=%q>%s</time>`,
		at.UTC().Format(time.RFC3339), at.Format("Mon Jan 2 15:04 MST"), at.Format(nextLayout(time.Now(), at))))
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
	served := s.servedTo(c)

	// A broken pipeline stops the shortcuts: redirected past, or shown the empty page, nobody would learn the daemon holds one it is not serving.
	broken := len(s.brokenTo(c)) > 0

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
	runs, err := s.recentRunsAcross(c.Request().Context(), served, limit+1)
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
// A few reads per pipeline, plus one per job strip, rather than one cross-pipeline query: a daemon holds a handful, they need not share a database, and a Reader can only answer what it can name. A read that fails leaves its field zero rather than failing the page — a root that will not render says less about a pipeline than a row missing one column.
func (s *Server) overviewPipelines(c *echo.Context, nav navData) []overviewPipeline {
	ctx := c.Request().Context()
	served := s.servedTo(c)

	summaries := make(map[string]pipelineSummary, len(nav.Pipelines))
	for _, summary := range nav.Pipelines {
		summaries[summary.Slug] = summary
	}

	out := make([]overviewPipeline, 0, len(served))

	for _, pipeline := range served {
		summary := summaries[pipeline.Slug]
		reads := readsOf(c, pipeline)
		row := overviewPipeline{
			Slug: pipeline.Slug,
			Jobs: len(pipeline.Config().Jobs),
			// Its own pause, not Paused: under PauseAll every row would say paused, and the row is where a reader sees what each returns to.
			Paused:    reads.PipelinePaused(ctx),
			Mark:      summary.Mark,
			Attention: summary.Attention,
			InFlight:  summary.InFlight,
			Chips:     jobChips(ctx, reads),
		}

		heldJob := map[string]bool{}

		held, err := reads.HeldJobs(ctx)
		if err == nil {
			row.Held = len(held)

			for _, job := range held {
				heldJob[job.Name] = true
			}
		}

		row.Next = nextFor(pipeline.Config(), heldJob, s.cronNext, time.Now())

		row.Latest, row.HasRun = redRun(row.Chips)
		if !row.HasRun {
			runs, err := pipeline.Store.ListRuns(ctx, "", 1)
			if err == nil && len(runs) > 0 {
				row.Latest, row.HasRun = runs[0], true
			}
		}

		out = append(out, row)
	}

	for _, broken := range s.brokenTo(c) {
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
func (s *Server) recentRunsAcross(ctx context.Context, served []*Pipeline, limit int) ([]overviewRun, error) {
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

	for _, pipeline := range served {
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
