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
// The server is single-user and binds loopback by default, where it asks for
// nothing: the thing it would authenticate against does not exist — this is
// the local runner's own UI, in the same trust domain as the terminal that
// started it. A deployment that is NOT that turns on HTTP Basic (see auth.go),
// which is the whole of the authentication here.
package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/scheduler"
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
const maxUploadSize int64 = 8 << 20

// maxBodySize bounds every other request body: net/http's form parsing would otherwise buffer 10MB per anonymous POST. /api keeps maxUploadSize and a webhook delivery is bounded by the trigger that reads it.
const maxBodySize int64 = 1 << 20

func hasOwnBodyLimit(c *echo.Context) bool {
	return strings.HasPrefix(c.Request().URL.Path, "/api/") || strings.HasSuffix(c.Path(), "/hooks/:resource")
}

// shutdownGrace is how long a cancelled daemon waits for the requests it is already answering.
const shutdownGrace = 5 * time.Second

// Pipeline is one held pipeline as this server sees it: the scheduler's handle (name, store, bus, served configuration) plus what only the HTTP surface needs.
type Pipeline struct {
	*scheduler.Pipeline
	// path names a file on the SENDER's machine, recorded for a reader who finds a configuration nobody remembers sending, and never opened here. Behind an atomic because a later set moves it while handlers read it.
	path atomic.Pointer[string]
	// access is the configuration's access: rules, compiled for the configuration they came from: see roles.go.
	access atomic.Pointer[compiledAccess]
	// Hooks answers POST /p/<slug>/hooks/<resource>, a delivery to a webhook resource. Built by the caller (trigger.HookHandler): this package serves the surface and does not own the queue — internal/scheduler drains it, reached only through Runner. Nil only in a test.
	Hooks func(w http.ResponseWriter, r *http.Request, resource string)
}

// NewPipeline builds a served pipeline around the configuration it starts
// with. A literal would leave the embedded handle nil.
func NewPipeline(slug, path string, cfg *config.Config, st store.Store, bus *events.Bus) *Pipeline {
	pipeline := &Pipeline{Pipeline: scheduler.NewPipeline(slug, cfg, st, bus)}
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

// Server serves whatever has been set into it. The registry is behind a lock rather than fixed at construction because a handler, the drain and the poll loop all read it while another request writes it.
type Server struct {
	mu        sync.RWMutex
	pipelines []*Pipeline
	bySlug    map[string]*Pipeline
	// broken are pipelines the database holds and a restart could not serve, by name: shown rather than dropped, because a daemon that looks healthy while missing one is the failure refusing to start used to prevent.
	broken map[string]BrokenPipeline
	echo   *echo.Echo
	// runner enqueues and executes jobs. nil disables every mutation a
	// BROWSER can reach, which is what a read-only deployment gets.
	//
	// The webhook route is deliberately not among them: it carries the
	// sender's own signature rather than riding this server's (absent)
	// authentication, and a read-only build box that could not be notified is
	// most of what a read-only build box is for. A pipeline that wants no such
	// endpoint declares no type: webhook resource, and the route 404s.
	runner Runner
	// capacity is nil when nothing told this server about --max-runs, which reads as no limit.
	capacity Capacity
	// renderer is held rather than only handed to echo because the live
	// stream renders one step with the SAME templates the page renders, which
	// is what keeps a row drawn live and a row drawn on reload identical.
	renderer *renderer
	// nil in a read-only server and in a test that only reads pages, where the API refuses rather than pretending to be unimplemented.
	manager Manager
	// nil is open access, which is what the loopback default is: see auth.go.
	auth *basicAuth
	// activity is where the activity log is read and how much of it is kept: see activity.go.
	activity activityLog
	// sessions is where signed-in browsers are kept, nil where browsers use the Basic prompt: see sessions.go.
	sessions store.Accounts
	// providers are the ways to sign in with somebody else's account, and pending the trips to them not yet back: see providers.go.
	providers []SignInProvider
	pending   pendingSignIns
	// roles are who may do what, the zero value letting anyone signed in do anything: see roles.go.
	roles Roles
	// external is the address the daemon is reached on, which every OAuth URL it publishes is built from; auths are consents and codes in flight: see oauthserver.go.
	external string
	auths    pendingAuth
	// exporter builds exports and exports holds the built ones: see exports.go.
	exporter Exporter
	exports  exportCache
	// nil answers any Host: see hosts.go. hostsImplied is a list nobody named, which a webhook delivery is exempt from.
	allowedHosts map[string]bool
	hostsImplied bool
	// version is what the footer prints, so a bug report can name the build.
	version string
	// tracing is the outermost middleware when set, so a request any later one refuses is still traced. Nil in page tests, which keep talking to a bare echo.
	tracing echo.MiddlewareFunc
}

// WithVersion is the build version the footer prints; "dev" when unset, the
// same default steps --version has.
func WithVersion(version string) Option {
	return func(s *Server) { s.version = version }
}

// WithCapacity is where the follow page reads --max-runs from. A separate option rather than part of Runner because a read-only server has no Runner and still drains under the same limit.
func WithCapacity(capacity Capacity) Option {
	return func(s *Server) { s.capacity = capacity }
}

// WithTracing installs m ahead of every other middleware; it is how internal/cli traces requests without this package importing a telemetry SDK.
func WithTracing(m echo.MiddlewareFunc) Option {
	return func(s *Server) { s.tracing = m }
}

// Capacity is the daemon-wide run limit (0 when unlimited) and how many builds hold it.
type Capacity interface {
	RunCapacity() (limit, active int)
}

// Manager applies what a `steps pipeline` verb asks for. An interface for the reason Runner is one: this package serves the surface and chooses neither a store driver nor a workspace provider.
type Manager interface {
	// Set applies a configuration under name, creating the pipeline if this
	// daemon does not hold one. req.ExpectSHA is the revision the sender
	// diffed against: empty means "I did not look", and a mismatch is refused
	// rather than applied over whatever arrived in between.
	Set(ctx context.Context, name string, req SetRequest) (SetResult, error)
	Destroy(ctx context.Context, name string) error
	Rename(ctx context.Context, from, to string) error
}

// SetRequest is one upload: the substituted YAML, the files it includes, and the revision the sender believed it was replacing.
type SetRequest struct {
	Source string `json:"source"`
	// A daemon has no sibling filesystem, so an include that does not travel here cannot be resolved at all (see config.Bundle); bytes rather than strings because encoding/json replaces each invalid UTF-8 byte of a string with U+FFFD, which ran a Latin-1 run_file: as bytes nobody sent.
	Includes map[string][]byte `json:"includes,omitempty"`
	// ExpectSHA is the compare-and-set. Empty means the sender did not look, which a script may legitimately do.
	ExpectSHA string `json:"expect_sha,omitempty"`
	// From is the SENDER's path, recorded for a reader wondering where a served configuration came from, and never opened here.
	From string `json:"from,omitempty"`
	// Pause is written with the revision, before the pipeline is started: a pause sent after the set is a second request, and the first poll — and whatever it triggers — can land between the two.
	Pause bool `json:"pause,omitempty"`
}

// SetResult is what the daemon did, so the terminal that asked can say so.
type SetResult struct {
	SHA string `json:"sha"`
	// Created, replaced and unchanged are three outcomes a person reads differently, so the answer says which.
	Created   bool `json:"created"`
	Unchanged bool `json:"unchanged"`
	// Repaired is a set that served a pipeline a restart had left broken.
	Repaired bool `json:"repaired,omitempty"`
}

// ErrRevisionMoved is a compare-and-set the daemon refused: the configuration moved between the sender's diff and its set.
var ErrRevisionMoved = errors.New("the pipeline's configuration changed since it was read")

// ErrNoSuchPipeline is a verb naming a pipeline this daemon does not hold.
var ErrNoSuchPipeline = errors.New("no such pipeline")

// Runner is what the web layer needs in order to act rather than only
// report: enqueue a job for execution, and report what is currently running.
// Implemented by scheduler.LocalRunner, which internal/cli installs; an interface so the
// HTTP layer can be tested without starting real jobs.
type Runner interface {
	// Enqueue queues a person's trigger of a job, returning the queue row id.
	Enqueue(ctx context.Context, pipeline *scheduler.Pipeline, jobName, reason string, force bool) (int64, error)
	// EnqueueRerun queues a retry of a recorded run.
	EnqueueRerun(ctx context.Context, pipeline *scheduler.Pipeline, jobName, runID string) error
	// Abort cancels a run this process is executing, and reports false when it is not running here.
	Abort(pipeline *scheduler.Pipeline, runID string) bool
	// AbortQueued drops a job's queued run before it starts, and reports false when nothing was queued.
	AbortQueued(ctx context.Context, pipeline *scheduler.Pipeline, jobName string) (bool, error)
}

// New builds a server over whatever pipelines it is handed, which may be none: a daemon is configured by `steps pipeline set` and by nothing else, so empty is the ordinary starting state rather than an error.
func New(pipelines []*Pipeline, runner Runner, opts ...Option) (*Server, error) {
	srv := &Server{bySlug: map[string]*Pipeline{}, broken: map[string]BrokenPipeline{}, runner: runner}

	// Before routes(), which is where the middleware table is fixed.
	for _, opt := range opts {
		opt(srv)
	}

	if len(srv.providers) > 0 {
		err := srv.roles.AllowsProviders()
		if err != nil {
			return nil, fmt.Errorf("web: %w", err)
		}
	}

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

// routes wires the handler table and the middleware every route shares.
func (s *Server) routes() error {
	e := echo.New()

	renderer, err := newRenderer(s.version)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	s.renderer = renderer
	e.Renderer = renderer
	e.HTTPErrorHandler = s.handleError

	if s.tracing != nil {
		e.Use(s.tracing)
	}

	e.Use(refuseFraming)
	e.Use(middleware.Recover())
	e.Use(middleware.BodyLimitWithConfig(middleware.BodyLimitConfig{LimitBytes: maxBodySize, Skipper: hasOwnBodyLimit}))

	// Before auth, so a rebinding page is refused before it is asked for a password it could phish.
	if s.allowedHosts != nil {
		e.Use(s.refuseOtherHosts)
	}
	e.Use(middleware.GzipWithConfig(middleware.GzipConfig{
		// A gzip writer holds bytes until a block fills, which is the one thing a live stream must not do; matched by route because htmx's sse extension adds text/event-stream to the Accept of every request.
		Skipper:   func(c *echo.Context) bool { return c.Path() == eventsRoute },
		MinLength: 1024,
	}))

	// Above the origin check, so an unauthenticated cross-origin request is told the one thing it can act on.
	if s.auth != nil {
		e.Use(s.authenticate)
		e.Use(s.authorize)
	}

	// With no authentication there is no session for a cross-site request to ride, but a page on another origin can still aim a form at localhost — and this is the check that costs nothing and closes it.
	e.Use(sameOriginMutations)

	// After auth, so an entry names who; around every route, so a mutating one cannot go unrecorded by being added without remembering to.
	e.Use(s.recordActivity)

	e.GET("/", s.handleIndex)
	e.GET("/static/app.css", s.handleCSS)
	e.GET("/static/htmx.min.js", s.handleHTMX)
	e.GET("/static/hx-sse.min.js", s.handleHTMXSSE)
	e.GET("/static/app.js", s.handleAppJS)
	e.GET("/mark", s.handleMark)
	e.GET("/docs", s.handleDocsIndex)
	e.GET("/docs/:page", s.handleDocs)
	e.GET("/activity", s.handleActivity)

	if s.sessions != nil {
		e.GET(loginPath, s.handleLoginPage)
		e.POST(loginPath, s.handleLogin)
		e.POST(logoutPath, s.handleLogout)
		e.GET(signInStartPath, s.handleSignInStart)
		e.GET(signInCallbackPath, s.handleSignInCallback)

		// Agents connecting to exports: metadata, registration and tokens answer the agent itself; authorize is a signed-in person's page.
		e.GET(authServerMetadataPath, s.handleAuthServerMetadata)
		e.GET(resourceMetadataPath, s.handleResourceMetadata)
		e.POST(registerPath, s.handleRegister)
		e.GET(authorizePath, s.handleAuthorize)
		e.POST(authorizePath, s.handleConsent)
		e.POST(tokenPath, s.handleToken)
		e.GET(connectionsPath, s.handleConnections)
		e.POST(revokePath, s.handleRevoke)
		// The methods MCP's streamable HTTP transport uses: POST a message, GET a stream, DELETE a session.
		e.POST(exportPath, s.handleExport)
		e.GET(exportPath, s.handleExport)
		e.DELETE(exportPath, s.handleExport)
	}

	group := e.Group("/p/:pipeline", s.resolvePipeline)
	group.GET("", s.handleJobs)
	group.GET("/jobs/:job", s.handleJobRedirect)
	group.GET("/jobs/:job/detail", s.handleJobDetail)
	group.GET("/runs", s.handleRunHistory)
	group.GET("/runs/:run", s.handleRun)
	group.GET("/mark", s.handleMark)
	group.GET(strings.TrimPrefix(runMarkRoute, "/p/:pipeline"), s.handleRunMark)
	group.GET(strings.TrimPrefix(eventsRoute, "/p/:pipeline"), s.handleRunEvents)
	group.GET(strings.TrimPrefix(turnRoute, "/p/:pipeline"), s.handleTurn)
	group.GET("/nodes/:hash", s.handleNode)
	group.GET("/config/:sha", s.handleConfig)
	group.GET("/approvals", s.handleApprovals)
	group.GET("/questions", s.handleQuestions)
	group.GET("/mcp", s.handleMCP)
	group.GET("/mcp/exports/:export", s.handleExportPage)
	group.GET("/resources", s.handleResources)
	group.GET("/resources/:resource", s.handleResource)
	group.GET("/search", s.handleSearch)
	group.GET("/jobs/:job/follow", s.handleFollow)
	group.GET("/jobs/:job/latest-run", s.handleLatestRun)

	group.POST("/jobs/:job/trigger", s.handleTrigger)
	group.POST("/runs/:run/rerun", s.handleRerun)
	group.POST("/approvals/:id", s.handleDecideApproval)
	group.POST("/questions/:id", s.handleAnswerQuestion)
	group.POST("/jobs/:job/release", s.handleRelease)
	// The pipeline-level breaker, reachable from a browser at last: /api holds the same two verbs and refuses a page by design, so the UI could say a pipeline was paused and not offer to release it. See docs/web.md.
	group.POST("/pause", s.handlePause)
	group.POST("/unpause", s.handleUnpause)
	group.POST("/runs/:run/abort", s.handleAbortRun)
	group.POST("/jobs/:job/queued/abort", s.handleAbortQueued)
	// Browser-reachable on purpose, and the one place this UI starts something outside itself. /api refuses anything browser-shaped, so until now a browser could not begin a login at all; what bounds it is that the pipeline already declares the server (adding one takes `steps pipeline set`, which is remote-shell-grade) and that sameOriginMutations refuses a cross-site POST. See docs/authentication.md.
	group.POST("/mcp/:server/connect", s.handleMCPConnect)
	group.POST("/mcp/:server/test", s.handleMCPTest)

	// Not a UI route: a webhook delivery. It authenticates with the sender's
	// signature, which is why it is exempt from the same-origin check every
	// browser mutation gets — a sender is cross-origin by definition.
	group.POST("/hooks/:resource", s.handleHook)

	// Outside the /p/ group because a set may CREATE the pipeline it names, and that middleware resolves one that exists.
	api := e.Group("/api", refuseBrowsers, middleware.BodyLimit(maxUploadSize))
	api.GET("/pipelines", s.handleAPIList)
	api.GET("/pipelines/:pipeline", s.handleAPIGet)
	api.PUT("/pipelines/:pipeline", s.handleAPISet)
	api.DELETE("/pipelines/:pipeline", s.handleAPIDestroy)
	api.POST("/pipelines/:pipeline/pause", s.handleAPIPause)
	api.POST("/pipelines/:pipeline/unpause", s.handleAPIUnpause)
	api.POST("/pipelines/:pipeline/rename", s.handleAPIRename)
	api.POST("/pipelines/:pipeline/runs/:run/abort", s.handleAPIAbortRun)
	api.POST("/pipelines/:pipeline/jobs/:job/queued/abort", s.handleAPIAbortQueued)
	api.POST("/pipelines/:pipeline/mcp/:server/login", s.handleAPIStartLogin)
	api.GET("/pipelines/:pipeline/mcp/:server/login", s.handleAPILoginStatus)

	// Top level rather than under /api, whose refuseBrowsers turns away exactly what this is: a browser, sent here by an authorization server.
	e.GET(MCPCallbackPath, s.handleMCPCallback)

	e.GET(HealthPath, func(c *echo.Context) error { return c.String(http.StatusOK, "ok") })

	s.echo = e

	return nil
}

// Handler exposes the router, for tests and for embedding.
func (s *Server) Handler() http.Handler { return s.echo }

// Start serves until ctx is canceled, then shuts down gracefully.
func (s *Server) Start(ctx context.Context, addr string) error {
	//nolint:wrapcheck // start and shutdown errors are reported verbatim by the caller
	return startConfig(addr).Start(ctx, s.echo)
}

// startConfig is the server this daemon runs, separate from Start so a test can read the timeouts back off it.
//
// echo builds the http.Server itself and no longer exposes it as a field, so BeforeServeFunc is the only reach into it.
func startConfig(addr string) echo.StartConfig {
	return echo.StartConfig{
		Address:         addr,
		HideBanner:      true,
		HidePort:        true,
		GracefulTimeout: shutdownGrace,
		BeforeServeFunc: func(server *http.Server) error {
			// A sender that stalls mid-request must not hold a connection open indefinitely, and this is the address a tunnel forwards webhook deliveries to.
			server.ReadHeaderTimeout = readHeaderTimeout

			// Explicitly NOT echo's own 30s default, which bounds the whole request: net/http starts a background read on a bodyless GET whose handler is still running, and that read hitting the deadline cancels the REQUEST CONTEXT (connReader.handleReadErrorLocked) — every live stream would drop and reconnect every 30 seconds, with the reader's fold lost each time.
			server.ReadTimeout = 0

			return nil
		},
	}
}

// handleError renders a failure as a page rather than echo's JSON default —
// this server only ever talks to a browser.
func (s *Server) handleError(c *echo.Context, err error) {
	if response, _ := echo.UnwrapResponse(c.Response()); response != nil && response.Committed {
		return
	}

	status := http.StatusInternalServerError
	message := "internal error"

	var httpErr *echo.HTTPError
	if errors.As(err, &httpErr) {
		status = httpErr.Code
		message = httpErr.Message
	} else if code := echo.StatusCode(err); code != 0 {
		// echo's own status sentinels carry a code and nothing else — an oversized `steps pipeline set` returns ErrStatusRequestEntityTooLarge, which errors.As does not see, and a 413 reported as 500 tells the terminal the daemon broke rather than that its upload was too big.
		status = code
	}

	// What a failing layer said is SQL and paths; the log keeps it and the viewer gets the fact.
	if status >= http.StatusInternalServerError {
		slog.ErrorContext(c.Request().Context(), "web.request_failed", "method", c.Request().Method, "path", c.Request().URL.Path, "status", status, "error", err)
	}

	// The API answers a terminal, not a browser, and a page of HTML where a
	// refusal was expected is a `steps pipeline set` that cannot say why it
	// was refused — which is the one thing this transport exists to do.
	if strings.HasPrefix(c.Path(), "/api/") {
		_ = c.JSON(status, map[string]string{"message": message})

		return
	}

	// A result's body is swapped into its box, and htmx swaps an error too:
	// a whole page there would nest the nav and its scripts in a <pre>.
	// A mark's carrier declines an error swap, so the body is never read;
	// empty keeps a page from being rendered for nobody.
	if isMarkRoute(c.Path()) {
		_ = c.NoContent(status)

		return
	}

	if c.Path() == turnRoute {
		_ = c.HTML(status, template.HTMLEscapeString(message))

		return
	}

	// globalNav, not nav: most errors fire where no pipeline resolved (a bad
	// slug, a stray 404), and bare nav leaves Current empty — every tab then
	// links /p//…, which 404s straight back into this handler.
	nav := s.unscopedNav(c)

	renderErr := c.Render(status, "error", map[string]any{
		"Nav":     nav,
		"Status":  status,
		"Message": message,
	})
	if renderErr != nil {
		_ = c.String(status, message)
	}
}
