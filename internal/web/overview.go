package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

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
		"Nav":       s.globalNav(c),
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
			Jobs: len(pipeline.Config().Jobs),
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
		handles = map[string]store.Store{}
		names   = map[string][]string{}
		// The store scopes itself by pipeline NAME and the URL is built from
		// the slug. They are the same string today — both come from
		// resolvePipelineName — but they are assigned in two places, and a
		// feed that assumed it would link every row somewhere wrong the day
		// that stopped being true.
		slugByName = map[string]string{}
	)

	for _, pipeline := range s.pipelines {
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

// agentDialView is one agent step's effective limits, as the job page shows
// them.
//
// The values are RESOLVED, not the ones written on any single object: a step
// may override its agent, the agent may override the pipeline default, and
// the default is a constant in Go. Answering "why did this step stop at 30
// turns" used to mean cross-referencing all three, which is why the numbers
// that decide it belong on the page that lists the step.
type agentDialView struct {
	// Where is the step this row describes, as VisitSteps labels it — the
	// plan position, and the hook when it is one. Without it two rows for one
	// agent are byte-identical, which defeats the section's own purpose: "why
	// did THIS step stop at N turns" is unanswerable when several rows claim
	// different Ns for the same name, and a hook agent that only runs on
	// failure reads as an ordinary plan step.
	Where string
	Agent string
	// Turns is the tool-calling cap. Zero means the author removed it.
	Turns int
	// ContextBytes caps what a context_paths: file may deliver. Zero means
	// uncapped, which only an explicit 0 produces.
	ContextBytes int
	// Timeout is the per-attempt deadline as written; empty means the
	// built-in default applies.
	Timeout string
	// BudgetTokens and BudgetUSD are the step's spend ceiling, in whichever
	// unit its runner can actually enforce. Never both: validation rejects
	// budget.tokens on a cli source and budget.usd on a hosted one, because
	// nothing counts a subprocess's tokens until it exits and no hosted
	// provider reports dollars. Zero in both means no ceiling.
	BudgetTokens int
	BudgetUSD    float64
	// BlockTokens is the across: block's own budget:, which every cell of the
	// matrix spends TOGETHER — a ceiling the pipeline enforces (it stops
	// admitting cells once it is spent) and one that lives on the step, not
	// the agent, so ResolveAgentInvocation never sees it. Zero for any step
	// that is not a budgeted matrix. Drawn beside the per-cell ceiling,
	// because "uncapped" alone on the one dial the runner does enforce read
	// as the step being held by its deadline and nothing else.
	BlockTokens int
	// Broken is why this step's invocation would not resolve, empty for the
	// ordinary case. A row with it carries no numbers, because there are
	// none.
	Broken string
}

// Uncapped reports a dial an author explicitly removed, which the page shows
// as a word rather than as 0 — a zero in a limit column reads as "nothing
// allowed", the opposite of what it means here.
func (a agentDialView) UncappedTurns() bool { return a.Turns == 0 }

// UncappedContext is the same for the context ceiling.
func (a agentDialView) UncappedContext() bool { return a.ContextBytes == 0 }

// UncappedBudget is the same for spend, and it is the one where the word
// carries the most weight. A cli agent's budget.usd is the ONLY ceiling its
// runner enforces mid-conversation — max_turns: is spent inside a subprocess
// this process cannot interrupt, and a job budget is tokens-only — so an
// uncapped row beside an uncapped turn count says the step is held by wall
// clock and nothing else. Per invocation: a matrix's cells may still be
// held together by BlockTokens, which the template draws beside this.
func (a agentDialView) UncappedBudget() bool { return a.BudgetTokens == 0 && a.BudgetUSD == 0 }

// Budget renders the ceiling in the unit it is metered in. One column rather
// than two: the spellings are mutually exclusive by source kind, so a second
// column would be empty on every row of a pipeline that does not mix them.
func (a agentDialView) Budget() string {
	if a.BudgetUSD > 0 {
		return FormatUSD(a.BudgetUSD)
	}

	return thousands(a.BudgetTokens) + " tokens"
}

// UncappedTimeout is the same for the deadline, and it is the one that had to
// be spelled out rather than left to the template.
//
// A deadline is a STRING here, and `timeout: 0` is the documented way to say
// a step has no wall-clock ceiling (agentTimeout returns noAgentDeadline for
// it). Any non-empty string is truthy in a Go template, so the most dangerous
// dial state on the page rendered as the smallest possible number — beside
// two columns that correctly said "uncapped" — and an operator auditing for
// runaway spend would read it as tightly bounded.
func (a agentDialView) UncappedTimeout() bool {
	if a.Timeout == "" {
		return false
	}

	parsed, err := config.ParseTimeout(a.Timeout)

	return err == nil && parsed == 0
}

// agentDials resolves the effective limits of every agent step in a job.
// It covers the agents a step names DIRECTLY, and an ensemble's `decide:`
// judge, which is an agent step the plan does not spell out as one. A task's
// `fix:` agent and a step's sub-agent `tools:` grants run real conversations
// under limits of their own and are not listed — Job.AgentNames walks all
// three, this walks one — so the heading says "steps", not "agents".
func agentDials(cfg *config.Config, job config.Job) []agentDialView {
	var dials []agentDialView

	// Through VisitSteps rather than over job.Plan, because a plan is a TREE:
	// do:, in_parallel:, across: and try: all hold steps, and an agent nested
	// in one runs under limits just as worth seeing.
	_ = job.VisitSteps(func(label string, step *config.Step) error {
		where := strings.TrimPrefix(label, fmt.Sprintf("job %q ", job.Name))

		// The judge is built at run time from decide: (runEnsembleJudge) and
		// runs — and records its spend — as an agent step of its own, so the
		// walk, which sees only the members, never yields it. Listed here or
		// its spend row on the run page reads "unknown", saying its name
		// resolves to no agent, about the one agent the block cannot load
		// without.
		if judge := step.Ensemble.JudgeAgent(); judge != "" {
			dials = append(dials, dialFor(cfg, where+" (judge)", config.Step{Agent: judge}))
		}

		if step.Agent == "" {
			return nil
		}

		dial := dialFor(cfg, where, *step)
		if step.Budget != nil {
			dial.BlockTokens = step.Budget.Tokens
		}

		dials = append(dials, dial)

		return nil
	})

	return dials
}

// dialFor is one step's row.
func dialFor(cfg *config.Config, where string, step config.Step) agentDialView {
	ri, err := cfg.ResolveAgentInvocation(step)
	if err != nil {
		// Shown, not dropped. Not every resolution failure is caught at
		// load — `reasoning_effort:` is validated here and nowhere else,
		// and an endpoint credential is deferred on purpose — so a
		// silently missing row would leave the page confidently listing
		// the job's other agents and looking complete while omitting the
		// one that is broken. That is the failure this section exists to
		// surface, not to hide.
		return agentDialView{Where: where, Agent: step.Agent, Broken: err.Error()}
	}

	return agentDialView{
		Where:        where,
		Agent:        ri.AgentName,
		Turns:        ri.MaxTurns,
		ContextBytes: ri.MaxContextBytes,
		Timeout:      ri.Timeout,
		BudgetTokens: ri.BudgetTokens,
		BudgetUSD:    ri.BudgetUSD,
	}
}

// agentDialValues resolves cfg/jobName/runSHA down to one map per AGENT name
// of whatever val reads off that agent's dial, gated on the run-configuration
// match every per-dial run-page fact shares (agentCeilings' spend ceiling,
// agentTimeouts' resolved wall-clock timeout, and any later one).
//
// The sha gate is the whole of the honesty here. The right answer would come
// from the run's OWN revision, and cannot today: a revision stores its source
// but not its include files, and internal/config loads from a path rather than
// from bytes, so a run older than the last edit is not reconstructable. The
// question a caller consults this for is "what was it capped/held to when it
// failed", which makes today's dial on last week's run the one wrong answer
// worse than no answer — so a mismatch reports drift (the true return) and no
// values, rather than falling through to whatever the loaded config says now.
//
// Keyed by AGENT name, which is what a dial belongs to (see
// config.ResolveAgentInvocation): a job running one agent twice gets one
// entry for both rows, which is right. agent_usage records a STEP name
// instead, and the two coincide for every step except an across: cell, which
// renames itself — lookupByCellName (model.go) is the side that knows to ask
// again without the cell's coordinates.
func agentDialValues[T any](cfg *config.Config, jobName, runSHA string, val func(agentDialView) T) (map[string]T, bool) {
	// Three states, and collapsing any two of them puts a wrong word on the
	// page. A run whose configuration is not the loaded one has dials that
	// cannot be known; a run whose configuration IS loaded may still have an
	// agent with no ceiling/deadline at all. "Unknowable" and "uncapped" are
	// opposite answers, so a run with no recorded sha reports the former
	// rather than falling through to the latter.
	if cfg == nil || cfg.Revision.SHA == "" || runSHA != cfg.Revision.SHA {
		return nil, true
	}

	job, err := cfg.FindJob(jobName)
	if err != nil {
		return nil, true
	}

	values := map[string]T{}

	for _, dial := range agentDials(cfg, *job) {
		if dial.Broken != "" {
			continue
		}

		values[dial.Agent] = val(dial)
	}

	return values, false
}

// agentCeilings is what each agent step of one job was allowed to spend, by
// step name, for a run that opened against the configuration now being
// served. See agentDialValues for the sha-drift gate and the agent-name key.
func agentCeilings(cfg *config.Config, jobName, runSHA string) (map[string]string, bool) {
	return agentDialValues(cfg, jobName, runSHA, func(dial agentDialView) string {
		// An uncapped agent is IN the map with an empty value, not left out
		// of it. Absence has to keep meaning "could not be resolved" — see
		// runView.ceilingFor — because a step whose name matches no agent
		// would otherwise render as uncapped, which is the opposite answer.
		if dial.UncappedBudget() {
			return ""
		}

		return dial.Budget()
	})
}

// agentTimeouts is what each agent step of one job would be held to on the
// currently loaded configuration, by AGENT name — the resolved wall-clock
// ceiling (config.ResolvedAgentTimeout, 0 meaning unlimited). See
// agentDialValues for the sha-drift gate this shares with agentCeilings.
func agentTimeouts(cfg *config.Config, jobName, runSHA string) (map[string]time.Duration, bool) {
	return agentDialValues(cfg, jobName, runSHA, func(dial agentDialView) time.Duration {
		return config.ResolvedAgentTimeout(dial.Timeout)
	})
}

// timeoutForStep resolves a step's agent/cell name against timeouts, sharing
// runView.ceilingFor's lookupByCellName (model.go) rather than a second copy
// of the same "<agent> [k=v]" reconciliation.
func timeoutForStep(timeouts map[string]time.Duration, stepName string) (time.Duration, bool) {
	return lookupByCellName(timeouts, stepName)
}

// attachStepDeadlines resolves each currently-running agent step's wall-clock
// deadline (Started + its resolved timeout:) so the run page can show a
// countdown alongside the elapsed time it already shows. Left at zero for
// every step when the run's configuration has drifted (agentTimeouts' own
// gate), for a non-agent or finished step, or for a step whose resolved
// timeout is unlimited.
//
// Called on BOTH of the run page's two rendering paths — handlers.go's
// assembleRun for a full load, and live.go's flushBatch for the SSE stream's
// persistent, incrementally-folded view — because the countdown is drawn
// inside the per-step row, which is exactly the part of the page the two
// paths share (see internal/web/CLAUDE.md, "the live view and the post-hoc
// view are the SAME rendering"). A run's own status and spend tables are the
// only things the stream is documented not to draw; this isn't one of them.
//
// That second call site is why every branch below explicitly ZEROES Deadline
// rather than merely skipping a step that doesn't qualify: assembleRun builds
// a fresh runView from scratch every call, so a `continue` and a zero start
// out identical there, but the SSE path's runFolder is long-lived and its
// *stepView pointers are reused across flushes — a step that had a deadline
// while running must have it cleared once it finishes, or its row still
// carries a stale "timeout in ..." the next time something else about it
// changes and the row is re-sent.
func attachStepDeadlines(view *runView, cfg *config.Config, jobName, runSHA string) {
	timeouts, drifted := agentTimeouts(cfg, jobName, runSHA)

	for _, step := range view.Steps {
		if drifted || step.Kind != "agent" || !step.Running() || step.Started.IsZero() {
			step.Deadline = time.Time{}

			continue
		}

		timeout, known := timeoutForStep(timeouts, step.Name)
		if !known || timeout == 0 {
			step.Deadline = time.Time{}

			continue
		}

		step.Deadline = step.Started.Add(timeout)
	}
}
