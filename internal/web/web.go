// Package web serves the pipeline UI: a read-and-operate view of what the
// runner has done and is doing, over the same sqlite store the CLI writes.
//
// It is a second front end on the existing model, never a second model. Every
// page answers a question the store already holds the answer to — what jobs
// exist and how they depend on each other, what a run did step by step, why a
// step was skipped, what an agent actually said — and the two mutations it
// offers (enqueue a job, decide an approval) go through the same rows
// `steps web` and `steps approvals approve` use. Nothing here is a parallel
// execution path.
//
// The server is single-user and binds loopback by default: there is no
// authentication, because the thing it authenticates against does not exist —
// this is the local runner's own UI, in the same trust domain as the terminal
// that started it.
package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// historyLimit bounds every history query. The UI is for looking at what
// happened recently; an operator wanting the whole table has sqlite.
const historyLimit = 200

// runEventLimit bounds one run's transcript, for the page and for the diff
// that reads a prior run's steps.
//
// A variable rather than a constant so a test can shrink it. The branch worth
// proving is that the STREAM does not inherit the page's bound — a flush past
// it once found no touched step and wrote nothing, freezing a live transcript
// — and that branch is reached by exceeding the bound, whatever it is. At
// 5,000 the only test that covers it had to write 5,013 events one
// transaction at a time, which cost more wall clock than the rest of the
// package put together and made the test the first thing to time out when the
// machine was busy.
//
//nolint:gochecknoglobals // a test seam for a bound no run reaches cheaply
var runEventLimit = 5000

// readHeaderTimeout bounds how long a client may take to send its headers.
// A page request and a webhook body are both small; a sender that dribbles
// them is holding a connection, not making a request.
const readHeaderTimeout = 5 * time.Second

// maxUploadSize bounds a `steps pipeline set`. It is the only unauthenticated write this server takes, and what it holds is buffered whole and then written into TEXT columns retention cannot reap while they are the current revision — every other free-text column here already declares a cap.
const maxUploadSize = "8M"

// Pipeline is one loaded pipeline the server serves, with its own config and
// its own store handle. Two served pipelines may now share a state FILE (see
// --db), but never a store handle: each one is scoped to its own pipeline
// row, which is what keeps their histories and caches apart.
type Pipeline struct {
	// Slug is the name `steps pipeline set` was given: the route, the store's pipelines.name and the Config's name are ONE identity, not three kept in agreement.
	Slug string
	// path names a file on the SENDER's machine, recorded for a reader who finds a configuration nobody remembers sending, and never opened here. Behind an atomic because a later set moves it while handlers read it.
	path atomic.Pointer[string]
	// cfg is the configuration currently being served, swapped when the file
	// on disk changes (see SetConfig). Unexported behind an atomic pointer
	// because every reader of it runs concurrently with that swap — handlers
	// serving requests, the queue drain starting a job, the trigger poller
	// deciding what to check — and a plain field read while the daemon
	// reloads is a data race, not a stale read.
	cfg   atomic.Pointer[config.Config]
	Store store.Store
	// Bus carries live run events for runs this process itself executes.
	// Runs started by a separate `steps run` land in the store but not on
	// this bus, which is why every live view falls back to replaying the
	// stored events rather than assuming the bus saw everything.
	Bus *events.Bus
	// Webhook serves POST /p/<slug>/check/<resource> for the resources this
	// pipeline gives a webhook_token_env:. Whether there are any is decided per
	// request, not here, because a reload can add or remove one; nil means only
	// that nobody attached a handler, which is a Pipeline built in a test.
	//
	// Built by the caller (trigger.WebhookHandler) rather than here: this
	// package serves the surface and does not own the poll loop, the same
	// division that keeps the runner an interface. It used to be a second
	// listener on a second port of the poll loop's own; one daemon means one
	// address.
	Webhook http.Handler
}

// NewPipeline builds a served pipeline around the configuration it starts
// with.
//
// A constructor rather than a struct literal because cfg is behind an atomic
// pointer: it is read by handlers, the drain and the poller at once, and a
// literal would leave it nil for whatever ran before the caller filled it in.
func NewPipeline(slug, path string, cfg *config.Config, st store.Store, bus *events.Bus) *Pipeline {
	pipeline := &Pipeline{Slug: slug, Store: st, Bus: bus}
	pipeline.cfg.Store(cfg)
	pipeline.path.Store(&path)

	return pipeline
}

// Path is where the served configuration was set from.
func (p *Pipeline) Path() string {
	path := p.path.Load()
	if path == nil {
		return ""
	}

	return *path
}

// SetPath records where a set that just landed was sent from.
func (p *Pipeline) SetPath(path string) { p.path.Store(&path) }

// Config is the configuration being served right now.
//
// Every reader goes through here rather than holding the pointer across a
// swap: a job started before a reload must run the plan it was queued
// against, which it does by taking this once, while the NEXT job takes the
// new one.
func (p *Pipeline) Config() *config.Config { return p.cfg.Load() }

// SetConfig swaps in a configuration a `steps pipeline set` just applied.
func (p *Pipeline) SetConfig(cfg *config.Config) { p.cfg.Store(cfg) }

// Server serves whatever has been set into it. The registry is behind a lock rather than fixed at construction because a handler, the drain and the poll loop all read it while another request writes it.
type Server struct {
	mu        sync.RWMutex
	pipelines []*Pipeline
	bySlug    map[string]*Pipeline
	echo      *echo.Echo
	// runner enqueues and executes jobs. nil disables every mutation a
	// BROWSER can reach, which is what a read-only deployment gets.
	//
	// The webhook route is deliberately not among them: it carries the
	// resource's own token rather than riding this server's (absent)
	// authentication, and a read-only build box that could not be notified is
	// most of what a read-only build box is for. A pipeline that wants no such
	// endpoint declares no webhook_token_env: resource, and the route 404s.
	runner Runner
	// renderer is held rather than only handed to echo because the live
	// stream renders one step with the SAME templates the page renders, which
	// is what keeps a row drawn live and a row drawn on reload identical.
	renderer *renderer
	// nil in a read-only server and in a test that only reads pages, where the API refuses rather than pretending to be unimplemented.
	manager Manager
}

// Manager applies what a `steps pipeline` verb asks for. An interface for the reason Runner is one: this package serves the surface and chooses neither a store driver nor a workspace provider.
type Manager interface {
	// Set applies a configuration under name, creating the pipeline if this
	// daemon does not hold one. expectSHA is the revision the sender diffed
	// against: empty means "I did not look", and a mismatch is refused rather
	// than applied over whatever arrived in between.
	Set(ctx context.Context, name string, req SetRequest) (SetResult, error)
	Destroy(ctx context.Context, name string) error
	Rename(ctx context.Context, from, to string) error
}

// SetRequest is one upload: the substituted YAML, the files it includes, and the revision the sender believed it was replacing.
type SetRequest struct {
	Source string `json:"source"`
	// A daemon has no sibling filesystem, so an include that does not travel here cannot be resolved at all — see config.Bundle.
	Includes map[string]string `json:"includes,omitempty"`
	// ExpectSHA is the compare-and-set. Empty means the sender did not look, which a script may legitimately do.
	ExpectSHA string `json:"expect_sha,omitempty"`
	// From is the SENDER's path, recorded for a reader wondering where a served configuration came from, and never opened here.
	From string `json:"from,omitempty"`
}

// SetResult is what the daemon did, so the terminal that asked can say so.
type SetResult struct {
	SHA string `json:"sha"`
	// Created, replaced and unchanged are three outcomes a person reads differently, so the answer says which.
	Created   bool `json:"created"`
	Unchanged bool `json:"unchanged"`
}

// ErrRevisionMoved is a compare-and-set the daemon refused: the configuration moved between the sender's diff and its set.
var ErrRevisionMoved = errors.New("the pipeline's configuration changed since it was read")

// ErrNoSuchPipeline is a verb naming a pipeline this daemon does not hold.
var ErrNoSuchPipeline = errors.New("no such pipeline")

// Runner is what the web layer needs in order to act rather than only
// report: enqueue a job for execution, and report what is currently running.
// Implemented by the in-process drainer (see runner.go); an interface so the
// HTTP layer can be tested without starting real jobs.
type Runner interface {
	// Enqueue queues a job for execution, returning the queue row id.
	Enqueue(ctx context.Context, pipeline *Pipeline, jobName, reason string, force bool) (int64, error)
}

// New builds a server over whatever pipelines it is handed, which may be none: a daemon is configured by `steps pipeline set` and by nothing else, so empty is the ordinary starting state rather than an error.
func New(pipelines []*Pipeline, runner Runner) (*Server, error) {
	srv := &Server{bySlug: map[string]*Pipeline{}, runner: runner}

	for _, pipeline := range pipelines {
		err := srv.Add(pipeline)
		if err != nil {
			return nil, err
		}
	}

	err := srv.routes()
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}

	return srv, nil
}

// SetManager is separate from New because the manager needs the server it registers into.
func (s *Server) SetManager(manager Manager) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.manager = manager
}

// Under the lock its sibling registry fields already take: an interface value read while another goroutine writes it is a torn method table, not a stale pointer.
func (s *Server) held() Manager {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.manager
}

// Add starts serving a pipeline, refusing a name already held.
func (s *Server) Add(pipeline *Pipeline) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, clash := s.bySlug[pipeline.Slug]; clash {
		return fmt.Errorf("web: this daemon already serves a pipeline called %q", pipeline.Slug)
	}

	s.bySlug[pipeline.Slug] = pipeline
	s.pipelines = append(s.pipelines, pipeline)

	sort.Slice(s.pipelines, func(i, j int) bool { return s.pipelines[i].Slug < s.pipelines[j].Slug })

	return nil
}

// Remove hands the pipeline back so the caller can shut down what it started for it, and nil when this daemon was not serving one.
func (s *Server) Remove(slug string) *Pipeline {
	s.mu.Lock()
	defer s.mu.Unlock()

	pipeline, held := s.bySlug[slug]
	if !held {
		return nil
	}

	delete(s.bySlug, slug)

	kept := s.pipelines[:0]

	for _, other := range s.pipelines {
		if other.Slug != slug {
			kept = append(kept, other)
		}
	}

	s.pipelines = kept

	return pipeline
}

// Lookup is the pipeline served under slug, nil when this daemon holds none.
//
// One return rather than the comma-ok a map gives, because the two say the
// same thing and a caller that reads the bool and keeps the pointer is exactly
// the shape a nil-flow analyzer cannot follow.
func (s *Server) Lookup(slug string) *Pipeline {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.bySlug[slug]
}

// Served is what this daemon holds right now, by slug.
func (s *Server) Served() []*Pipeline {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return append([]*Pipeline(nil), s.pipelines...)
}

// routes wires the handler table and the middleware every route shares.
func (s *Server) routes() error {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	renderer, err := newRenderer()
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	s.renderer = renderer
	e.Renderer = renderer
	e.HTTPErrorHandler = s.handleError

	// echo builds a bare http.Server, which has no read timeouts at all. The
	// webhook listener this address absorbed set one explicitly, for a reason
	// that survived the merge: a sender that stalls mid-request must not hold
	// a connection open indefinitely, and this is the address an operator
	// exposes to receive deliveries from outside.
	e.Server.ReadHeaderTimeout = readHeaderTimeout

	e.Use(middleware.Recover())

	// Every mutation is a POST whose Origin must match the host serving it.
	// With no authentication there is no session for a cross-site request to
	// ride, but a page on another origin can still aim a form at localhost —
	// and this is the check that costs nothing and closes it.
	e.Use(sameOriginMutations)

	e.GET("/", s.handleIndex)
	e.GET("/static/app.css", s.handleCSS)
	e.GET("/static/htmx.min.js", s.handleHTMX)
	e.GET("/static/hx-sse.min.js", s.handleHTMXSSE)
	e.GET("/docs", s.handleDocsIndex)
	e.GET("/docs/:page", s.handleDocs)

	group := e.Group("/p/:pipeline", s.resolvePipeline)
	group.GET("", s.handleJobs)
	group.GET("/jobs/:job", s.handleJob)
	group.GET("/runs", s.handleRunHistory)
	group.GET("/runs/:run", s.handleRun)
	group.GET("/runs/:run/events", s.handleRunEvents)
	group.GET("/nodes/:hash", s.handleNode)
	group.GET("/config/:sha", s.handleConfig)
	group.GET("/approvals", s.handleApprovals)
	group.GET("/questions", s.handleQuestions)
	group.GET("/resources", s.handleResources)
	group.GET("/resources/:resource", s.handleResource)
	group.GET("/search", s.handleSearch)
	group.GET("/jobs/:job/follow", s.handleFollow)
	group.GET("/jobs/:job/latest-run", s.handleLatestRun)

	group.POST("/jobs/:job/trigger", s.handleTrigger)
	group.POST("/approvals/:id", s.handleDecideApproval)
	group.POST("/questions/:id", s.handleAnswerQuestion)
	group.POST("/jobs/:job/resume", s.handleResumeBreaker)

	// Not a UI route: an outside system saying "check this resource now".
	// It authenticates with the resource's own token, which is why it is
	// exempt from the same-origin check every browser mutation gets — a
	// webhook sender is cross-origin by definition.
	group.POST("/check/:resource", s.handleWebhook)

	// Outside the /p/ group because a set may CREATE the pipeline it names, and that middleware resolves one that exists.
	api := e.Group("/api", middleware.BodyLimit(maxUploadSize))
	api.GET("/pipelines", s.handleAPIList)
	api.GET("/pipelines/:pipeline", s.handleAPIGet)
	api.PUT("/pipelines/:pipeline", s.handleAPISet)
	api.DELETE("/pipelines/:pipeline", s.handleAPIDestroy)
	api.POST("/pipelines/:pipeline/pause", s.handleAPIPause)
	api.POST("/pipelines/:pipeline/unpause", s.handleAPIUnpause)
	api.POST("/pipelines/:pipeline/rename", s.handleAPIRename)

	s.echo = e

	return nil
}

// Handler exposes the router, for tests and for embedding.
func (s *Server) Handler() http.Handler { return s.echo }

// Start serves until ctx is canceled, then shuts down gracefully.
func (s *Server) Start(ctx context.Context, addr string) error {
	errs := make(chan error, 1)

	go func() {
		err := s.echo.Start(addr)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err

			return
		}

		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		//nolint:wrapcheck // shutdown error is reported verbatim by the caller
		return s.echo.Shutdown(shutdownCtx)
	}
}

// resolvePipeline is the middleware that turns :pipeline into the loaded
// pipeline every handler under /p/ works from.
func (s *Server) resolvePipeline(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		pipeline := s.Lookup(c.Param("pipeline"))
		if pipeline == nil {
			return echo.NewHTTPError(http.StatusNotFound, "no such pipeline")
		}

		c.Set("pipeline", pipeline)

		return next(c)
	}
}

// pipelineOf returns the pipeline resolved for this request.
func pipelineOf(c echo.Context) *Pipeline {
	pipeline, _ := c.Get("pipeline").(*Pipeline)

	return pipeline
}

// sameOriginMutations rejects a state-changing request whose Origin is not
// the host it was sent to. Safe methods pass untouched, and a request with no
// Origin header at all passes too — that is a curl or a CLI, not a browser
// being aimed at this port by another page.
func sameOriginMutations(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		method := c.Request().Method
		if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
			return next(c)
		}

		// A webhook is authenticated by its token and sent by a machine that
		// has no reason to share this origin. Exempting it here rather than
		// mounting it outside the group keeps it under /p/<slug>/, which is
		// what says which pipeline it checks.
		if strings.Contains(c.Path(), "/check/:resource") {
			return next(c)
		}

		origin := c.Request().Header.Get("Origin")
		if origin == "" {
			return next(c)
		}

		if origin != "http://"+c.Request().Host && origin != "https://"+c.Request().Host {
			return echo.NewHTTPError(http.StatusForbidden, "cross-origin request refused")
		}

		return next(c)
	}
}

// handleError renders a failure as a page rather than echo's JSON default —
// this server only ever talks to a browser.
func (s *Server) handleError(err error, c echo.Context) {
	if c.Response().Committed {
		return
	}

	status := http.StatusInternalServerError
	message := err.Error()

	var httpErr *echo.HTTPError
	if errors.As(err, &httpErr) {
		status = httpErr.Code
		message = fmt.Sprintf("%v", httpErr.Message)
	}

	// The API answers a terminal, not a browser, and a page of HTML where a
	// refusal was expected is a `steps pipeline set` that cannot say why it
	// was refused — which is the one thing this transport exists to do.
	if strings.HasPrefix(c.Path(), "/api/") {
		_ = c.JSON(status, map[string]string{"message": message})

		return
	}

	// globalNav, not nav: most errors fire where no pipeline resolved (a bad
	// slug, a stray 404), and bare nav leaves Current empty — every tab then
	// links /p//…, which 404s straight back into this handler.
	renderErr := c.Render(status, "error", map[string]any{
		"Nav":     s.globalNav(c),
		"Status":  status,
		"Message": message,
	})
	if renderErr != nil {
		_ = c.String(status, message)
	}
}

// nav is the shell every page renders inside: which pipelines exist, which
// one is current, and the counts the top bar reports.
// globalNav is nav() for a page that has no current pipeline: the overview
// and /docs both sit above `/p/:pipeline`, so pipelineOf is nil there and the
// shell's tabs, switcher and jump palette would render dead "/p//" links —
// which 404, and which the palette's own error handling then swallows, so it
// simply shows nothing.
//
// Anchoring them to the first pipeline keeps the way back into the app alive.
// It is a display fallback only: nothing on either page is scoped by it.
func (s *Server) globalNav(c echo.Context) navData {
	nav := s.nav(c)

	if nav.Current == "" && len(nav.Pipelines) > 0 {
		nav.Current = nav.Pipelines[0].Slug
		nav.CurrentPath = nav.Pipelines[0].Path
	}

	return nav
}

func (s *Server) nav(c echo.Context) navData {
	nav := navData{ReadOnly: s.runner == nil, URL: c.Request().URL.RequestURI()}

	for _, pipeline := range s.Served() {
		nav.Pipelines = append(nav.Pipelines, pipelineSummary{
			Slug: pipeline.Slug,
			Path: pipeline.Path(),
			Jobs: len(pipeline.Config().Jobs),
		})
	}

	sort.Slice(nav.Pipelines, func(i, j int) bool {
		return nav.Pipelines[i].Slug < nav.Pipelines[j].Slug
	})

	current := pipelineOf(c)
	if current == nil {
		return nav
	}

	nav.Current = current.Slug
	nav.CurrentPath = current.Path()
	nav.Paused = paused(c.Request().Context(), current)

	pending, err := current.Store.Approvals(c.Request().Context(), true, 0)
	if err == nil {
		nav.PendingApprovals = len(pending)
	}

	questions, err := current.Store.Questions(c.Request().Context(), true, 0)
	if err == nil {
		nav.PendingQuestions = len(questions)
	}

	return nav
}

// navData is the top-bar model.
type navData struct {
	Pipelines   []pipelineSummary
	Current     string
	CurrentPath string
	// URL is the URL this page was requested at, which is what the live
	// regions poll: htmx needs a URL on the element, and the page rendering
	// itself is the one thing that reliably knows its own. Query included —
	// the poll has to reproduce the page, and the day one of these pages
	// takes a filter, a path-only re-fetch would reset it every 2.5s.
	URL              string
	PendingApprovals int
	PendingQuestions int
	ReadOnly         bool
	// On the nav because it is a fact about every page: a board of green jobs that has stopped moving has to say why.
	Paused bool
}

// Answers false when the store cannot say: a page that fails to draw is worse than a missing banner.
func paused(ctx context.Context, target *Pipeline) bool {
	if target.Store == nil {
		return false
	}

	is, err := target.Store.Paused(ctx)

	return err == nil && is
}

type pipelineSummary struct {
	Slug string
	Path string
	Jobs int
}
