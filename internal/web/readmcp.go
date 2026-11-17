package web

// The daemon's read-only MCP server at /mcp: internal/mcpread holds the tools, and this file decides who a request is from and what they may see, then answers the tools' questions as them. An agent sees exactly what its person would in the browser — a pipeline whose access: hides it from them is hidden here, as not found — and that is asked on every call, so leaving a team or a set that narrows a pipeline ends access at the very next one.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/expr-lang/expr/vm"
	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/internal/actor"
	"github.com/jtarchie/steps/internal/mcpread"
	"github.com/jtarchie/steps/internal/runview"
	"github.com/jtarchie/steps/internal/store"
)

const (
	// readPath is the read server's one address, and so the RFC 8707 resource a read token is for.
	readPath             = "/mcp"
	readResourceMetaPath = "/.well-known/oauth-protected-resource/mcp"
	// readAction is what every call is recorded as.
	readAction = "mcp read"
	// maxReadOutput bounds how much of one step's output a call reads to page through, every attempt's output together: past it the text ends in readCutNote rather than pretending to be whole.
	maxReadOutput = 32 << 20
	// readRunDepth is how many runs search and get_cost read back.
	readRunDepth = 50
)

// WithMCPAccess is --mcp-access: who may connect an agent to read at all, in the --role rule language. Without it, anyone with the viewer role may.
func WithMCPAccess(program *vm.Program) Option {
	return func(s *Server) { s.mcpAccess = program }
}

// CompileMCPAccess compiles an --mcp-access rule; an empty one is no rule.
func CompileMCPAccess(rule string) (*vm.Program, error) {
	if strings.TrimSpace(rule) == "" {
		return nil, nil //nolint:nilnil // no rule is no program, and no error
	}

	program, err := compileRule(rule)
	if err != nil {
		return nil, fmt.Errorf("--mcp-access: %w", err)
	}

	return program, nil
}

func (s *Server) readResource() string { return s.external + readPath }

// mayRead is whether a person may read through an agent at all: somebody this daemon still lets in, with at least the viewer role, whom --mcp-access admits. Which pipelines they see is asked per pipeline, per call.
func (s *Server) mayRead(ctx context.Context, who actor.Actor) (Identity, bool) {
	if !s.signsIn(who) {
		return Identity{}, false
	}

	id := s.identityOf(ctx, who)
	if s.roles.highest(id) < RoleViewer {
		return Identity{}, false
	}

	if s.mcpAccess != nil && !holds(s.mcpAccess, id) {
		return Identity{}, false
	}

	return id, true
}

// bearerReadGrant is the grant a request's access token was issued under, when that is a live read token. An export's token is refused here, as a read token is at every export.
func (s *Server) bearerReadGrant(c *echo.Context) (store.Grant, bool) {
	token, ok := s.bearerToken(c)
	if !ok || token.Grant.Kind != store.GrantRead {
		return store.Grant{}, false
	}

	return token.Grant, true
}

// bearerToken is the live access token the request carries, of either kind.
func (s *Server) bearerToken(c *echo.Context) (store.OAuthToken, bool) {
	raw, found := strings.CutPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Bearer ")
	if !found || raw == "" || s.sessions == nil {
		return store.OAuthToken{}, false
	}

	token, live, err := s.sessions.FindToken(c.Request().Context(), tokenDigest(raw))
	if err != nil || !live || token.Kind != store.TokenAccess {
		return store.OAuthToken{}, false
	}

	return token, true
}

// handleRead answers an agent's MCP request. A daemon nobody signs in to answers it as it answers its pages, with no sign-in; any other needs a read token, and asks again whether its person may read.
func (s *Server) handleRead(c *echo.Context) error {
	ctx := c.Request().Context()
	source := readSource{server: s}

	if s.auth != nil {
		grant, ok := s.bearerReadGrant(c)
		if !ok {
			c.Response().Header().Set(echo.HeaderWWWAuthenticate, `Bearer resource_metadata="`+s.external+readResourceMetaPath+`"`)

			return echo.ErrUnauthorized
		}

		who := actor.Parse(grant.Actor)

		id, allowed := s.mayRead(ctx, who)
		if !allowed {
			return echo.NewHTTPError(http.StatusForbidden, "access to read this daemon has ended")
		}

		source.who, source.id = who, id
		ctx = actor.With(ctx, who)
	}

	s.reader.ServeHTTP(c.Response(), c.Request().WithContext(mcpread.With(ctx, source)))

	return nil
}

func (s *Server) handleReadResourceMetadata(c *echo.Context) error {
	//nolint:wrapcheck // echo writes the response
	return c.JSON(http.StatusOK, map[string]any{
		"resource":                 s.readResource(),
		"authorization_servers":    []string{s.external},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "steps (read-only)",
	})
}

// readSource answers the tools as one person: who and id are zero on a daemon nobody signs in to, whose rules (none) let anyone see everything.
type readSource struct {
	server *Server
	who    actor.Actor
	id     Identity
}

var _ mcpread.Source = readSource{}

// sees is the browser's own question, asked now: the daemon's role for this person, narrowed by the pipeline's access: rules.
func (r readSource) sees(p *Pipeline) bool {
	return p.narrow(r.server.roles.highest(r.id), r.id) >= RoleViewer
}

// visible is the named pipeline, or not found when there is none or the person may not see it — one answer for both, so an agent cannot learn a hidden pipeline exists.
func (r readSource) visible(name string) (*Pipeline, error) {
	p := r.server.Lookup(name)
	if p == nil || !r.sees(p) {
		return nil, mcpread.ErrNotFound
	}

	return p, nil
}

func (r readSource) Pipelines(ctx context.Context) ([]mcpread.Pipeline, error) {
	var out []mcpread.Pipeline

	for _, p := range r.server.Served() {
		if !r.sees(p) {
			continue
		}

		out = append(out, mcpread.Pipeline{Name: p.Slug, Paused: paused(ctx, p), Jobs: len(p.Config().Jobs)})
	}

	return out, nil
}

func (r readSource) Pipeline(ctx context.Context, name string) (mcpread.PipelineDetail, error) {
	p, err := r.visible(name)
	if err != nil {
		return mcpread.PipelineDetail{}, err
	}

	latest, err := p.Store.LatestRunByJob(ctx)
	if err != nil {
		return mcpread.PipelineDetail{}, fmt.Errorf("web: %w", err)
	}

	held, err := p.Store.HeldJobs(ctx)
	if err != nil {
		return mcpread.PipelineDetail{}, fmt.Errorf("web: %w", err)
	}

	detail := mcpread.PipelineDetail{Name: p.Slug, Paused: paused(ctx, p)}

	revision, found, err := p.Store.CurrentRevision(ctx)
	if err == nil && found {
		detail.CurrentSHA = revision.SHA
	}

	for _, job := range p.Config().Jobs {
		row := mcpread.Job{Name: job.Name, Held: slices.ContainsFunc(held, func(h store.HeldJob) bool { return h.Name == job.Name })}

		if run, ran := latest[job.Name]; ran {
			summary := runSummary(run)
			row.Latest = &summary
		}

		detail.Jobs = append(detail.Jobs, row)
	}

	return detail, nil
}

func stamp(at time.Time) string {
	if at.IsZero() {
		return ""
	}

	return at.UTC().Format(time.RFC3339)
}

func runSummary(run store.RunRow) mcpread.Run {
	return mcpread.Run{
		ID: run.ID, Job: run.JobName, Status: string(run.Status), StartedAt: stamp(run.StartedAt), FinishedAt: stamp(run.FinishedAt),
		CreatedBy: run.CreatedBy, RerunOf: run.RerunOf, ConfigSHA: run.ConfigSHA,
	}
}

func (r readSource) Config(ctx context.Context, pipeline, sha, run string) (mcpread.Config, error) {
	p, err := r.visible(pipeline)
	if err != nil {
		return mcpread.Config{}, err
	}

	if run != "" {
		row, found, err := p.Store.FindRunRow(ctx, run)
		if err != nil {
			return mcpread.Config{}, fmt.Errorf("web: %w", err)
		}

		if !found || row.ConfigSHA == "" {
			return mcpread.Config{}, mcpread.ErrNotFound
		}

		sha = row.ConfigSHA
	}

	var (
		revision store.Revision
		found    bool
	)

	if sha == "" {
		revision, found, err = p.Store.CurrentRevision(ctx)
	} else {
		revision, found, err = p.Store.FindRevision(ctx, sha)
	}

	if err != nil {
		return mcpread.Config{}, fmt.Errorf("web: %w", err)
	}

	if !found {
		return mcpread.Config{}, mcpread.ErrNotFound
	}

	return mcpread.Config{SHA: revision.SHA, Source: revision.Source, Includes: revision.Includes}, nil
}

func (r readSource) Runs(ctx context.Context, pipeline string, q mcpread.RunQuery) ([]mcpread.Run, error) {
	p, err := r.visible(pipeline)
	if err != nil {
		return nil, err
	}

	rows, err := p.Store.FindRuns(ctx, store.RunQuery{Job: q.Job, Before: q.Before, Limit: q.Limit})
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}

	runs := make([]mcpread.Run, len(rows))
	for i, row := range rows {
		runs[i] = runSummary(row)
	}

	return runs, nil
}

// transcript is a run folded the way its page folds it, for the steps; node results are not read, since no tool shows one.
func transcript(ctx context.Context, p *Pipeline, id string) (runview.Transcript, error) {
	run, found, err := p.Store.FindRunRow(ctx, id)
	if err != nil {
		return runview.Transcript{}, fmt.Errorf("web: %w", err)
	}

	if !found {
		return runview.Transcript{}, mcpread.ErrNotFound
	}

	// Paged, as the live stream pages: one read of runEventLimit would end a long agent run at its 5,000th event, and a step past it — often the one that failed — would not be there to ask about.
	folder := runview.NewFolder()

	for after := int64(0); ; {
		rows, err := p.Store.RunEvents(ctx, run.ID, after, runEventLimit)
		if err != nil {
			return runview.Transcript{}, fmt.Errorf("web: %w", err)
		}

		folder.Add(rows, nil)

		if len(rows) < runEventLimit {
			break
		}

		after = rows[len(rows)-1].Seq
	}

	return folder.View(run), nil
}

func (r readSource) Run(ctx context.Context, pipeline, id string) (mcpread.RunDetail, error) {
	p, err := r.visible(pipeline)
	if err != nil {
		return mcpread.RunDetail{}, err
	}

	view, err := transcript(ctx, p, id)
	if err != nil {
		return mcpread.RunDetail{}, err
	}

	detail := mcpread.RunDetail{Run: runSummary(view.Run), Steps: make([]mcpread.Step, 0, len(view.Steps))}

	for _, step := range view.Steps {
		row := readStep(step)
		detail.Steps = append(detail.Steps, row)

		if detail.FailedStep == nil && step.Failed() && !slices.ContainsFunc(step.Children, func(child *runview.Step) bool { return child.Failed() }) {
			failed := row
			detail.FailedStep = &failed
		}
	}

	return detail, nil
}

func readStep(step *runview.Step) mcpread.Step {
	return mcpread.Step{
		ID: step.ID, ParentID: step.ParentID, Name: step.Name, Kind: step.Kind, Status: step.Status, Error: step.Error, Reason: step.Reason,
		DurationMS: step.Duration.Milliseconds(), Printed: len(step.Outputs) > 0 || len(step.Streaming) > 0,
	}
}

func (r readSource) StepOutput(ctx context.Context, pipeline, run string, stepID int64) (string, error) {
	p, err := r.visible(pipeline)
	if err != nil {
		return "", err
	}

	view, err := transcript(ctx, p, run)
	if err != nil {
		return "", err
	}

	index := slices.IndexFunc(view.Steps, func(step *runview.Step) bool { return step.ID == stepID })
	if index < 0 {
		return "", mcpread.ErrNotFound
	}

	step := view.Steps[index]
	out := &boundedText{limit: r.server.readOutputLimit()}

	for i, printed := range step.Outputs {
		if (i > 0 && !out.add("\n")) || !out.addOutput(ctx, p, view.Run.ID, step, i, printed) {
			break
		}
	}

	for _, chunk := range step.Streaming {
		if !out.add(chunk) {
			break
		}
	}

	return string(out.buf), nil
}

// errReadFull is a step's output past what one call reads.
var errReadFull = errors.New("the output is longer than one read holds")

// boundedText is one call's read of a step's output, which stops at limit and says so in the text, so a cut log is never mistaken for a whole one.
type boundedText struct {
	buf   []byte
	limit int
	full  bool
}

// readCutNote ends an output cut at the read limit.
const readCutNote = "\n[steps: the output continues past what one read holds; the rest is stored, and the run's page links it whole]\n"

// add appends text, or as much of it as fits followed by readCutNote; false once nothing more fits.
func (b *boundedText) add(text string) bool {
	if b.full {
		return false
	}

	if len(b.buf)+len(text) > b.limit {
		b.buf = append(append(b.buf, text[:max(b.limit-len(b.buf), 0)]...), readCutNote...)
		b.full = true

		return false
	}

	b.buf = append(b.buf, text...)

	return true
}

// addOutput appends output i of step: its stored whole when the event kept only part, else what the event recorded.
func (b *boundedText) addOutput(ctx context.Context, p *Pipeline, runID string, step *runview.Step, i int, printed string) bool {
	if n, kept := keptOutputN(step, i); kept {
		found, err := p.Store.StepOutput(ctx, runID, step.ID, n, func(chunk []byte) error {
			if !b.add(string(chunk)) {
				return errReadFull
			}

			return nil
		})
		if errors.Is(err, errReadFull) {
			return false
		}

		if err == nil && found {
			return true
		}
	}

	return b.add(printed)
}

func (r readSource) Search(ctx context.Context, pipeline, text string) ([]mcpread.Hit, error) {
	p, err := r.visible(pipeline)
	if err != nil {
		return nil, err
	}

	var hits []mcpread.Hit

	for _, job := range p.Config().Jobs {
		if strings.Contains(strings.ToLower(job.Name), strings.ToLower(text)) {
			hits = append(hits, mcpread.Hit{Kind: "job", Name: job.Name})
		}
	}

	runs, err := p.Store.FindRuns(ctx, store.RunQuery{Text: text, Limit: readRunDepth})
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}

	for _, run := range runs {
		hits = append(hits, mcpread.Hit{Kind: "run", Name: run.JobName, RunID: run.ID, Status: string(run.Status)})
	}

	return hits, nil
}

func (r readSource) Waiting(ctx context.Context, pipeline string) ([]mcpread.Waiting, error) {
	p, err := r.visible(pipeline)
	if err != nil {
		return nil, err
	}

	approvals, err := p.Store.Approvals(ctx, true, 0)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}

	questions, err := p.Store.Questions(ctx, true, 0)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}

	waiting := make([]mcpread.Waiting, 0, len(approvals)+len(questions))

	for _, a := range approvals {
		waiting = append(waiting, mcpread.Waiting{Pipeline: p.Slug, Kind: "approval", ID: a.ID, RunID: a.RunID, Job: a.JobName, Message: a.Message, Since: stamp(a.RequestedAt)})
	}

	for _, q := range questions {
		waiting = append(waiting, mcpread.Waiting{Pipeline: p.Slug, Kind: "question", ID: q.ID, RunID: q.RunID, Job: q.JobName, Agent: q.AgentName, Message: q.Question, Options: q.Options, Since: stamp(q.AskedAt)})
	}

	return waiting, nil
}

func (r readSource) Cost(ctx context.Context, pipeline, run string) (mcpread.Cost, error) {
	p, err := r.visible(pipeline)
	if err != nil {
		return mcpread.Cost{}, err
	}

	if run == "" {
		totals, err := p.Store.RunCostTotals(ctx, readRunDepth)
		if err != nil {
			return mcpread.Cost{}, fmt.Errorf("web: %w", err)
		}

		cost := mcpread.Cost{Runs: make([]mcpread.RunCost, len(totals))}
		for i, total := range totals {
			cost.Runs[i] = mcpread.RunCost{Run: total.RunID, Tokens: total.Tokens, Cached: total.Cached, CostUSD: total.CostUSD, Unpriced: total.Unpriced}
		}

		return cost, nil
	}

	row, found, err := p.Store.FindRunRow(ctx, run)
	if err != nil {
		return mcpread.Cost{}, fmt.Errorf("web: %w", err)
	}

	if !found {
		return mcpread.Cost{}, mcpread.ErrNotFound
	}

	usage, err := p.Store.RunUsage(ctx, row.ID)
	if err != nil {
		return mcpread.Cost{}, fmt.Errorf("web: %w", err)
	}

	cost := mcpread.Cost{Run: row.ID, Steps: make([]mcpread.StepCost, len(usage))}
	for i, spent := range usage {
		cost.Steps[i] = mcpread.StepCost{Step: spent.StepName, Model: spent.ModelServed, Tokens: spent.Total, Cached: spent.Cached, CostUSD: spent.CostUSD, DurationMS: spent.DurationMS}
	}

	return cost, nil
}

func (r readSource) Resources(ctx context.Context, pipeline string) ([]mcpread.Resource, error) {
	p, err := r.visible(pipeline)
	if err != nil {
		return nil, err
	}

	checked, err := p.Store.CheckedResources(ctx)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}

	failing, err := p.Store.CheckErrors(ctx)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}

	byName, failingBy := checkedByName(checked), failingByName(failing)
	resources := make([]mcpread.Resource, 0, len(p.Config().Resources))

	for _, res := range p.Config().Resources {
		row := mcpread.Resource{Name: res.Name, Type: res.Type, CheckError: failingBy[res.Name].Message}

		if seen, ok := byName[res.Name]; ok {
			row.LastChecked, row.Version = stamp(seen.CheckedAt), string(seen.Version)
		}

		resources = append(resources, row)
	}

	return resources, nil
}

func (r readSource) Versions(ctx context.Context, pipeline, resource string) ([]string, error) {
	p, err := r.visible(pipeline)
	if err != nil {
		return nil, err
	}

	res, err := p.Config().FindResource(resource)
	if err != nil {
		return nil, mcpread.ErrNotFound
	}

	lines, err := p.Store.ResourceVersionsJSON(ctx, res.Name)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}

	versions := make([]string, len(lines))
	for i, line := range lines {
		versions[len(lines)-1-i] = string(line)
	}

	return versions, nil
}

// Record writes an activity entry per pipeline the call read, naming the tool and nothing the agent sent — at most one per person, tool and pipeline in readCoalesce. The log is bounded by count across the whole daemon, and an agent polling list_pipelines over fifty pipelines would otherwise push every person's trigger and approval out of it within the hour; a repeat of a read already on record says nothing new.
func (r readSource) Record(ctx context.Context, tool string, pipelines []string) {
	for _, name := range pipelines {
		p := r.server.Lookup(name)
		if p == nil || !r.server.readsSeen.first(r.who.String()+"\x00"+tool+"\x00"+name, time.Now()) {
			continue
		}

		err := p.Store.RecordActivity(context.WithoutCancel(ctx), store.ActivityEntry{Actor: r.who.String(), Action: readAction, Target: tool}, r.server.activityCap())
		if err != nil {
			slog.ErrorContext(ctx, "web.activity_unrecorded", "pipeline", p.Slug, "action", readAction, "error", err)
		}
	}
}

// readCoalesce is how long one recorded read stands for its repeats in the activity log.
const readCoalesce = 10 * time.Minute

// maxReadsSeen bounds the coalescing memory; past it the oldest are forgotten, which costs only an extra entry.
const maxReadsSeen = 10000

// recentReads remembers when each read was last recorded.
type recentReads struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// first is whether key has not been recorded within readCoalesce of now, marking it recorded if so.
func (r *recentReads) first(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if at, ok := r.seen[key]; ok && now.Sub(at) < readCoalesce {
		return false
	}

	if r.seen == nil || len(r.seen) >= maxReadsSeen {
		r.sweep(now)
	}

	r.seen[key] = now

	return true
}

func (r *recentReads) sweep(now time.Time) {
	kept := make(map[string]time.Time, len(r.seen))

	for key, at := range r.seen {
		if now.Sub(at) < readCoalesce && len(kept) < maxReadsSeen/2 {
			kept[key] = at
		}
	}

	r.seen = kept
}

// readOutputLimit is how much of one step's output a call reads.
func (s *Server) readOutputLimit() int {
	if s.readCap > 0 {
		return s.readCap
	}

	return maxReadOutput
}
