package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/jtarchie/steps/internal/store"
)

// handleJobs renders the board: every job, its latest run, the trigger queue.
func (s *Server) handleJobs(c echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	latest, err := pipeline.Store.LatestRunByJob(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	paused, err := pipeline.Store.PausedJobs(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	queue, err := pipeline.Store.ListTriggerQueue(ctx, 25)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "jobs", map[string]any{
		"Nav":   s.nav(c),
		"Jobs":  buildJobViews(pipeline.Cfg, latest, paused),
		"Queue": pendingQueue(queue),
	})
}

// pendingQueue filters the queue to what is still outstanding — a finished
// row is history, and history is what the run list is for.
func pendingQueue(rows []store.QueueRow) []store.QueueRow {
	var out []store.QueueRow

	for _, row := range rows {
		if row.Status == "pending" || row.Status == "running" {
			out = append(out, row)
		}
	}

	return out
}

// handleJob renders one job: its dependencies, its runs, its green versions.
func (s *Server) handleJob(c echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()
	name := c.Param("job")

	job, err := pipeline.Cfg.FindJob(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("no job %q in this pipeline", name))
	}

	latest, err := pipeline.Store.LatestRunByJob(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	paused, err := pipeline.Store.PausedJobs(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	runs, err := pipeline.Store.ListRuns(ctx, job.Name, historyLimit)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	versions, err := pipeline.Store.PassedVersions(ctx, job.Name, 25)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	// The job's own view carries both edge directions; find it rather than
	// recomputing, so the board and this page can never disagree.
	var view jobView

	for _, candidate := range buildJobViews(pipeline.Cfg, latest, paused) {
		if candidate.Name == job.Name {
			view = candidate
		}
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "job", map[string]any{
		"Nav":      s.nav(c),
		"Title":    job.Name,
		"Job":      view,
		"Runs":     runs,
		"Versions": versions,
		"Spark":    sparkline(runs),
		"Dials":    agentDials(pipeline.Cfg, *job),
	})
}

// handleRun renders a run transcript.
func (s *Server) handleRun(c echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	run, ok, err := pipeline.Store.FindRunRow(ctx, c.Param("run"))
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "no such run")
	}

	view, err := s.assembleRun(c, run)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	// A failed run opens by answering the question it prompts: what is
	// different about this one. Only computed for a failure — on a green run
	// it is trivia, and it costs a second query.
	if view.Run.Status == "failed" {
		err = s.attachDiff(c, &view)
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "run", map[string]any{
		"Nav":       s.nav(c),
		"Run":       view,
		"Title":     view.Run.JobName,
		"TitleMark": statusMark(view.Run.Status),
	})
}

// assembleRun reads a run's events and the nodes they reference, and folds
// them into the view.
func (s *Server) assembleRun(c echo.Context, run store.RunRow) (runView, error) {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	rows, err := pipeline.Store.RunEvents(ctx, run.ID, 0, runEventLimit)
	if err != nil {
		return runView{}, fmt.Errorf("web: %w", err)
	}

	// Deduped: a hash repeats across a run's events (and across the steps a
	// chain-skip swallowed), and each repeat would add a redundant bind
	// parameter to the IN clause below.
	seen := map[string]bool{}
	hashes := make([]string, 0, len(rows))

	for _, row := range rows {
		if row.Hash == "" || seen[row.Hash] {
			continue
		}

		seen[row.Hash] = true

		hashes = append(hashes, row.Hash)
	}

	nodes, err := pipeline.Store.NodesByHash(ctx, hashes)
	if err != nil {
		return runView{}, fmt.Errorf("web: %w", err)
	}

	view := buildRunView(run, rows, nodes)

	// Best-effort: a run page that cannot show spend is worth more than one
	// that 500s over it. A run predating this table simply has none.
	usage, err := pipeline.Store.RunUsage(ctx, run.ID)
	if err == nil {
		view.Usage = usage
	}

	// Same terms: a run page that cannot say which machines it used is worth
	// more than one that 500s over it, and a run predating this table has none.
	placements, err := pipeline.Store.RunPlacements(ctx, run.ID)
	if err == nil {
		view.Placements = placements
	}

	return view, nil
}

// attachDiff fills in what changed since the last green run of this job.
func (s *Server) attachDiff(c echo.Context, view *runView) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	runs, err := pipeline.Store.ListRuns(ctx, view.Run.JobName, historyLimit)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	for _, candidate := range runs {
		if candidate.Status != "succeeded" || !candidate.StartedAt.Before(view.Run.StartedAt) {
			continue
		}

		prior, err := s.priorSteps(c, candidate)
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}

		view.Changed = diffAgainst(*view, prior)
		view.ComparedTo = candidate.ID

		return nil
	}

	return nil
}

// priorSteps reads only what a diff compares: the prior run's step names and
// the content hashes beside them.
//
// The full page assembler would answer three more queries — the nodes behind
// those hashes, the run's spend, the machines it used — and diffAgainst
// discards every one of them. buildRunView takes nil results for exactly this
// reason: a hash comes off the event row, and only a step's rendered RESULT
// needs the node.
func (s *Server) priorSteps(c echo.Context, run store.RunRow) (runView, error) {
	rows, err := pipelineOf(c).Store.RunEvents(c.Request().Context(), run.ID, 0, runEventLimit)
	if err != nil {
		return runView{}, fmt.Errorf("web: %w", err)
	}

	return buildRunView(run, rows, nil), nil
}

// handleNode renders one merkle node: what its hash is made of, and which
// runs have used it. It is the cache's receipt — the answer to "why was this
// step skipped", which nothing else in the product can show.
func (s *Server) handleNode(c echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()
	hash := c.Param("hash")

	node, ok, err := pipeline.Store.FindNode(ctx, hash)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "no node with that hash")
	}

	runs, err := pipeline.Store.RunsUsingNode(ctx, hash, 25)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	transcript, hasTranscript, err := pipeline.Store.NodeTranscript(ctx, hash)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "node", map[string]any{
		"Nav":        s.nav(c),
		"Node":       node,
		"Content":    node.Content,
		"Result":     node.Result,
		"Runs":       runs,
		"Transcript": transcriptEvents(transcript, hasTranscript),
	})
}

// handleApprovals lists decisions, pending first.
func (s *Server) handleApprovals(c echo.Context) error {
	pipeline := pipelineOf(c)

	approvals, err := pipeline.Store.AllApprovals(c.Request().Context(), historyLimit)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "approvals", map[string]any{
		"Nav":       s.nav(c),
		"Approvals": approvals,
	})
}

// handleQuestions lists what agents have asked: everything still waiting
// first, then the rest newest-first (see AllQuestions for why the order is not
// simply recency).
func (s *Server) handleQuestions(c echo.Context) error {
	pipeline := pipelineOf(c)

	questions, err := pipeline.Store.AllQuestions(c.Request().Context(), historyLimit)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "questions", map[string]any{
		"Nav":       s.nav(c),
		"Questions": questions,
	})
}

// handleResources shows what the watcher has seen, and the breaker state that
// stops it acting on what it sees.
func (s *Server) handleResources(c echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	checked, err := pipeline.Store.CheckedResources(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	paused, err := pipeline.Store.PausedJobs(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "resources", map[string]any{
		"Nav":       s.nav(c),
		"Resources": pipeline.Cfg.Resources,
		"Checked":   checkedByName(checked),
		"Paused":    paused,
	})
}

func checkedByName(rows []store.CheckedResource) map[string]store.CheckedResource {
	byName := map[string]store.CheckedResource{}
	for _, row := range rows {
		byName[row.Name] = row
	}

	return byName
}

// handleTrigger queues a job. force re-runs everything, ignoring the merkle
// cache — without it a re-run of an unchanged pipeline correctly does almost
// nothing, which is never what someone pressing "re-run" meant.
func (s *Server) handleTrigger(c echo.Context) error {
	if s.runner == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server is read-only")
	}

	pipeline := pipelineOf(c)
	name := c.Param("job")

	_, err := pipeline.Cfg.FindJob(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("no job %q in this pipeline", name))
	}

	force := c.FormValue("force") != ""

	reason := "manual (web)"
	if force {
		reason = "manual re-run, forced (web)"
	}

	// Stamped BEFORE enqueueing: the run this click causes must start at or
	// after this instant, and a stamp taken afterwards could miss a run the
	// drainer started in between.
	since := time.Now().UTC()

	_, err = s.runner.Enqueue(c.Request().Context(), pipeline, name, reason, force)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	// Follow, do not return to the list. The natural loop is trigger then
	// watch; sending the browser back to a table it must poll by hand leaves
	// the person to do the waiting the UI is for.
	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, fmt.Sprintf("/p/%s/jobs/%s/follow?since=%d",
		pipeline.Slug, name, since.UnixMilli()))
}

// handleFollow is the waiting room between enqueueing a job and its run
// existing. A queued job has no run id until a worker claims it, so the page
// reports what the queue is doing and forwards itself the moment the run
// appears.
func (s *Server) handleFollow(c echo.Context) error {
	pipeline := pipelineOf(c)
	name := c.Param("job")

	_, err := pipeline.Cfg.FindJob(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("no job %q in this pipeline", name))
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "follow", map[string]any{
		"Nav":       s.nav(c),
		"Title":     name,
		"TitleMark": statusMark("running"),
		"Job":       name,
		"Since":     c.QueryParam("since"),
	})
}

// handleLatestRun answers the follow page: has a run of this job started
// since the given millisecond stamp, and what is the queue doing meanwhile.
func (s *Server) handleLatestRun(c echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()
	name := c.Param("job")

	// A missing or unparseable stamp means no run has been credited to this
	// page yet, so nothing that already exists can be the answer. Falling back
	// to zero would make FirstRunSince match the job's entire history and
	// forward to its OLDEST run, presenting an ancient transcript as the one
	// just triggered.
	//
	// The fallback is now+1ms, not now, because UnixMilli TRUNCATES: a run
	// started earlier in the current millisecond has a started_at greater than a
	// millisecond-floored "now", so it satisfies `>= since` and gets credited to
	// a page that never triggered anything. Rounding up is the direction that
	// matches the intent — a sentinel later than everything that already exists.
	//
	// It only surfaced when runs.started_at became zero-padded (see
	// store.sortableNano): before that, comparing '…123456789Z' against '…123Z'
	// as text rejected the run by accident of the trimming, not by design.
	millis, err := strconv.ParseInt(c.QueryParam("since"), 10, 64)
	if err != nil || millis <= 0 {
		millis = time.Now().UTC().UnixMilli() + 1
	}

	since := time.UnixMilli(millis).UTC()

	run, ok, findErr := pipeline.Store.FirstRunSince(ctx, name, since)
	if findErr != nil {
		return fmt.Errorf("web: %w", findErr)
	}

	if ok {
		//nolint:wrapcheck // echo's JSON error is returned verbatim
		return c.JSON(http.StatusOK, map[string]any{
			"run": run.ID,
			"url": fmt.Sprintf("/p/%s/runs/%s", pipeline.Slug, run.ID),
		})
	}

	// No run yet. Say why, so a job held by a serial group or sitting behind
	// a busy drainer reads as queued rather than as nothing happening.
	queue, queueErr := pipeline.Store.ListTriggerQueue(ctx, 25)
	if queueErr != nil {
		return fmt.Errorf("web: %w", queueErr)
	}

	state := "waiting"

	for _, row := range queue {
		if row.JobName == name && (row.Status == "pending" || row.Status == "running") {
			state = row.Status

			break
		}
	}

	//nolint:wrapcheck // echo's JSON error is returned verbatim
	return c.JSON(http.StatusOK, map[string]any{"run": nil, "state": state})
}

// handleDecideApproval records a human decision, through the same row the
// CLI's approve/reject write.
func (s *Server) handleDecideApproval(c echo.Context) error {
	if s.runner == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server is read-only")
	}

	pipeline := pipelineOf(c)

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "approval id must be a number")
	}

	status := "approved"
	if c.FormValue("decision") == "reject" {
		status = "rejected"
	}

	// A decision that no longer matches a pending row is a resubmission — a
	// double click, or a refresh re-POSTing the form. The first one recorded
	// the audit trail correctly, so settle on it rather than reporting a 500
	// for work that succeeded.
	err = pipeline.Store.DecideApproval(c.Request().Context(), id, status, "web", c.FormValue("reason"))
	if err != nil {
		slog.Info("web.approval_not_pending", "id", id, "error", err)
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, "/p/"+pipeline.Slug+"/approvals")
}

// handleAnswerQuestion records an answer, through the same row `steps answer`
// writes — including its options fence, which lives in the store precisely so
// that this handler cannot be the place it is forgotten.
func (s *Server) handleAnswerQuestion(c echo.Context) error {
	if s.runner == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server is read-only")
	}

	pipeline := pipelineOf(c)

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "question id must be a number")
	}

	// An option button wins over the free-text field, which is what somebody
	// who clicked one meant even if they had typed something first.
	answer := strings.TrimSpace(c.FormValue("answer"))
	if answer == "" {
		answer = strings.TrimSpace(c.FormValue("answer_text"))
	}

	if answer == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "an answer is required")
	}

	// Two failures, and only one of them is the caller's business. A question
	// somebody else just answered (or that expired while this form sat open) is
	// a resubmission, exactly like a double-clicked approval: the row is the
	// answer of record and it already has one, so settle on it.
	//
	// A REFUSED answer is different and must be shown. Redirecting on an
	// options-fence rejection left the question listed as pending with no
	// message anywhere, so the person retypes the same thing or concludes the
	// step is stuck — while the agent is still waiting on them.
	err = pipeline.Store.AnswerQuestion(c.Request().Context(), id, answer, "web")

	switch {
	case err == nil:
	case errors.Is(err, store.ErrQuestionNotPending):
		slog.Info("web.question_already_resolved", "id", id, "error", err)
	default:
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, "/p/"+pipeline.Slug+"/questions")
}

// handleResumeBreaker puts a paused job back in the watch rotation.
func (s *Server) handleResumeBreaker(c echo.Context) error {
	if s.runner == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server is read-only")
	}

	pipeline := pipelineOf(c)

	err := pipeline.Store.ResetJobFailures(c.Request().Context(), c.Param("job"))
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, "/p/"+pipeline.Slug+"/resources")
}

// searchHit is one palette entry. Hint is where the answer to "which one?"
// goes: a status for a run, and the pipeline for anything that lives outside
// the one being searched from.
//
// match is what the query is tested against, and it is NOT the rendered
// fields. Filtering on Hint made the same query answer differently depending
// on which page it was typed on — a slug appears in the hint only for OTHER
// pipelines, so searching a pipeline's name worked everywhere except that
// pipeline's own page, which is where an operator is most likely to type it.
type searchHit struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Hint string `json:"hint"`
	URL  string `json:"url"`

	// match is the haystack, always including the owning pipeline. Unexported
	// so it never reaches the client, and so a future field cannot join the
	// filter just by being rendered.
	match string `json:"-"`
}

const (
	// searchHitLimit bounds the palette. It is a control, not a report — past
	// a screenful the answer is a better query.
	searchHitLimit = 20
	// searchRunDepth is how far back each pipeline is asked for runs. Per
	// pipeline rather than overall, so adding a second pipeline never makes
	// the first one's history shallower.
	searchRunDepth = 50
)

// handleSearch backs the jump palette: jobs, recent runs, and pipelines,
// filtered by substring, across every pipeline this process serves. JSON
// rather than a page — it is the one place the UI is a control instead of a
// document.
func (s *Server) handleSearch(c echo.Context) error {
	pipeline := pipelineOf(c)
	query := strings.ToLower(c.QueryParam("q"))

	var hits []searchHit

	full := func() bool { return len(hits) >= searchHitLimit }

	add := func(candidate searchHit) {
		if full() {
			return
		}

		if query != "" && !strings.Contains(strings.ToLower(candidate.match), query) {
			return
		}

		hits = append(hits, candidate)
	}

	// Ordered by KIND across every pipeline, not pipeline by pipeline. That
	// is the whole reason this reaches other pipelines at all: with one
	// pipeline's jobs AND its runs emitted before the next was considered,
	// searchRunDepth runs against a searchHitLimit cap meant the current
	// pipeline's own history filled every slot, and a pipeline with twenty
	// recent runs never showed a neighbour again. Jobs are few and are what
	// somebody is usually jumping to; runs are many and are the tail.
	order := append([]*Pipeline{pipeline}, s.others(pipeline)...)

	for _, other := range order {
		add(searchHit{
			Kind:  "pipeline",
			Name:  other.Slug,
			Hint:  other.Path,
			URL:   "/p/" + other.Slug,
			match: "pipeline " + other.Slug + " " + other.Path,
		})
	}

	for _, other := range order {
		addJobHits(add, other, other != pipeline)
	}

	for _, other := range order {
		if full() {
			// Nothing left to fill, and a run query is the expensive part:
			// the palette refetches on every keystroke, and under one shared
			// --state file these serialize on the same connection the runner
			// writes events through.
			break
		}

		s.addRunHits(c.Request().Context(), add, other, other != pipeline)
	}

	//nolint:wrapcheck // echo's JSON error is returned verbatim
	return c.JSON(http.StatusOK, hits)
}

// others is every served pipeline except the one given, in slug order.
func (s *Server) others(current *Pipeline) []*Pipeline {
	out := make([]*Pipeline, 0, len(s.pipelines))

	for _, pipeline := range s.pipelines {
		if pipeline != current {
			out = append(out, pipeline)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })

	return out
}

// addJobHits offers one pipeline's jobs to the palette.
//
// elsewhere marks a hit that lives in a pipeline other than the one being
// searched from, which the hint has to say: two pipelines may legitimately
// hold a job of the same name, and a palette offering both with nothing to
// tell them apart is worse than one that never found the second.
func addJobHits(add func(searchHit), pipeline *Pipeline, elsewhere bool) {
	where := ""
	if elsewhere {
		where = pipeline.Slug
	}

	for _, job := range pipeline.Cfg.Jobs {
		add(searchHit{
			Kind:  "job",
			Name:  job.Name,
			Hint:  where,
			URL:   fmt.Sprintf("/p/%s/jobs/%s", pipeline.Slug, job.Name),
			match: "job " + job.Name + " " + pipeline.Slug,
		})
	}
}

// addRunHits offers one pipeline's recent runs to the palette.
//
// A store that will not answer is LOGGED and skipped rather than failing the
// request. The palette is navigation: before it spanned pipelines a sibling
// could not break it, and 500ing the whole thing because a pipeline nobody
// was looking at hit a busy database would take out the primary way around
// the UI for every pipeline at once — the client has no rejection handler, so
// it would silently freeze on stale results.
//
// Read through each pipeline's OWN scoped handle rather than a cross-pipeline
// reader: search is a concatenation of per-pipeline lists, not one ordered
// feed, so there is nothing here a scoped read cannot answer.
func (s *Server) addRunHits(ctx context.Context, add func(searchHit), pipeline *Pipeline, elsewhere bool) {
	runs, err := pipeline.Store.ListRuns(ctx, "", searchRunDepth)
	if err != nil {
		slog.Warn("web.search.runs_unavailable", "pipeline", pipeline.Slug, "error", err)

		return
	}

	for _, run := range runs {
		hint := run.Status
		if elsewhere {
			hint = pipeline.Slug + " " + run.Status
		}

		add(searchHit{
			Kind:  "run",
			Name:  run.JobName + " " + shortID(run.ID),
			Hint:  hint,
			URL:   fmt.Sprintf("/p/%s/runs/%s", pipeline.Slug, run.ID),
			match: "run " + run.JobName + " " + run.ID + " " + run.Status + " " + pipeline.Slug,
		})
	}
}
