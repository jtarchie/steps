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
const runEventLimit = 5000

// readHeaderTimeout bounds how long a client may take to send its headers.
// A page request and a webhook body are both small; a sender that dribbles
// them is holding a connection, not making a request.
const readHeaderTimeout = 5 * time.Second

// Pipeline is one loaded pipeline the server serves, with its own config and
// its own store handle. Two served pipelines may now share a state FILE (see
// --state), but never a store handle: each one is scoped to its own pipeline
// row, which is what keeps their histories and caches apart.
type Pipeline struct {
	// Slug is the URL-safe identity a route carries, and the same string the
	// state database records as this pipeline's name — one identity, not two
	// that have to be kept in agreement. It is the YAML's base name without
	// extension unless --name overrides it, and two pipelines resolving to one
	// slug is refused at startup (see New).
	Slug  string
	Path  string
	Cfg   *config.Config
	Store *store.Store
	// Bus carries live run events for runs this process itself executes.
	// Runs started by a separate `steps run` land in the store but not on
	// this bus, which is why every live view falls back to replaying the
	// stored events rather than assuming the bus saw everything.
	Bus *events.Bus
	// Webhook serves POST /p/<slug>/check/<resource> for the resources this
	// pipeline gave a webhook_token_env:, and is nil when it gave none.
	//
	// Built by the caller (trigger.WebhookHandler) rather than here: this
	// package serves the surface and does not own the poll loop, the same
	// division that keeps the runner an interface. It used to be a second
	// listener on a second port of the poll loop's own; one daemon means one
	// address.
	Webhook http.Handler
}

// Server serves one or more pipelines.
type Server struct {
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
}

// Runner is what the web layer needs in order to act rather than only
// report: enqueue a job for execution, and report what is currently running.
// Implemented by the in-process drainer (see runner.go); an interface so the
// HTTP layer can be tested without starting real jobs.
type Runner interface {
	// Enqueue queues a job for execution, returning the queue row id.
	Enqueue(ctx context.Context, pipeline *Pipeline, jobName, reason string, force bool) (int64, error)
}

// New builds a server over already-loaded pipelines. runner may be nil, in
// which case the UI is read-only and says so.
func New(pipelines []*Pipeline, runner Runner) (*Server, error) {
	if len(pipelines) == 0 {
		return nil, errors.New("web: no pipelines to serve")
	}

	srv := &Server{pipelines: pipelines, bySlug: map[string]*Pipeline{}, runner: runner}

	for _, pipeline := range pipelines {
		if _, clash := srv.bySlug[pipeline.Slug]; clash {
			return nil, fmt.Errorf("web: two pipelines resolve to the same slug %q", pipeline.Slug)
		}

		srv.bySlug[pipeline.Slug] = pipeline
	}

	err := srv.routes()
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}

	return srv, nil
}

// Slugify turns a pipeline path into its URL identity.
//
// Delegated rather than computed, because the /p/<slug> route, the store's
// pipelines.name and the Config's own name are ONE identity: a second copy of
// this three-line function is how they came apart before, and two copies that
// agree today are two copies that can stop agreeing.
func Slugify(path string) string {
	return config.Slugify(path)
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
	e.GET("/docs", s.handleDocsIndex)
	e.GET("/docs/:page", s.handleDocs)

	group := e.Group("/p/:pipeline", s.resolvePipeline)
	group.GET("", s.handleJobs)
	group.GET("/jobs/:job", s.handleJob)
	group.GET("/runs/:run", s.handleRun)
	group.GET("/runs/:run/events", s.handleRunEvents)
	group.GET("/nodes/:hash", s.handleNode)
	group.GET("/approvals", s.handleApprovals)
	group.GET("/questions", s.handleQuestions)
	group.GET("/resources", s.handleResources)
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
		pipeline, ok := s.bySlug[c.Param("pipeline")]
		if !ok {
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

	renderErr := c.Render(status, "error", map[string]any{
		"Nav":     s.nav(c),
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
	nav := navData{ReadOnly: s.runner == nil}

	for _, pipeline := range s.pipelines {
		nav.Pipelines = append(nav.Pipelines, pipelineSummary{
			Slug: pipeline.Slug,
			Path: pipeline.Path,
			Jobs: len(pipeline.Cfg.Jobs),
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
	nav.CurrentPath = current.Path

	pending, err := current.Store.PendingApprovals(c.Request().Context())
	if err == nil {
		nav.PendingApprovals = len(pending)
	}

	questions, err := current.Store.PendingQuestions(c.Request().Context())
	if err == nil {
		nav.PendingQuestions = len(questions)
	}

	return nav
}

// navData is the top-bar model.
type navData struct {
	Pipelines        []pipelineSummary
	Current          string
	CurrentPath      string
	PendingApprovals int
	PendingQuestions int
	ReadOnly         bool
}

type pipelineSummary struct {
	Slug string
	Path string
	Jobs int
}
