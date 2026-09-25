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
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

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
const maxUploadSize int64 = 8 << 20

// shutdownGrace is how long a cancelled daemon waits for the requests it is already answering.
const shutdownGrace = 5 * time.Second

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
	// Hooks answers POST /p/<slug>/hooks/<resource>, a delivery to a webhook resource. Built by the caller (trigger.HookHandler): this package serves the surface and does not own the queue, the division that keeps the runner an interface. Nil only in a test.
	Hooks func(w http.ResponseWriter, r *http.Request, resource string)
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
	// sender's own signature rather than riding this server's (absent)
	// authentication, and a read-only build box that could not be notified is
	// most of what a read-only build box is for. A pipeline that wants no such
	// endpoint declares no type: webhook resource, and the route 404s.
	runner Runner
	// renderer is held rather than only handed to echo because the live
	// stream renders one step with the SAME templates the page renders, which
	// is what keeps a row drawn live and a row drawn on reload identical.
	renderer *renderer
	// nil in a read-only server and in a test that only reads pages, where the API refuses rather than pretending to be unimplemented.
	manager Manager
	// nil is open access, which is what the loopback default is: see auth.go.
	auth *basicAuth
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
	// Abort cancels a run this process is executing, and reports false when it is not running here.
	Abort(pipeline *Pipeline, runID string) bool
	// AbortQueued drops a job's queued run before it starts, and reports false when nothing was queued.
	AbortQueued(ctx context.Context, pipeline *Pipeline, jobName string) (bool, error)
}

// New builds a server over whatever pipelines it is handed, which may be none: a daemon is configured by `steps pipeline set` and by nothing else, so empty is the ordinary starting state rather than an error.
func New(pipelines []*Pipeline, runner Runner, opts ...Option) (*Server, error) {
	srv := &Server{bySlug: map[string]*Pipeline{}, runner: runner}

	// Before routes(), which is where the middleware table is fixed.
	for _, opt := range opts {
		opt(srv)
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

	renderer, err := newRenderer()
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	s.renderer = renderer
	e.Renderer = renderer
	e.HTTPErrorHandler = s.handleError

	e.Use(middleware.Recover())

	// Above the origin check, so an unauthenticated cross-origin request is told the one thing it can act on.
	if s.auth != nil {
		e.Use(s.auth.middleware())
	}

	// With no authentication there is no session for a cross-site request to ride, but a page on another origin can still aim a form at localhost — and this is the check that costs nothing and closes it.
	e.Use(sameOriginMutations)

	e.GET("/", s.handleIndex)
	e.GET("/static/app.css", s.handleCSS)
	e.GET("/static/htmx.min.js", s.handleHTMX)
	e.GET("/static/hx-sse.min.js", s.handleHTMXSSE)
	e.GET("/docs", s.handleDocsIndex)
	e.GET("/docs/:page", s.handleDocs)

	group := e.Group("/p/:pipeline", s.resolvePipeline)
	group.GET("", s.handleJobs)
	group.GET("/jobs/:job", s.handleJobRedirect)
	group.GET("/jobs/:job/detail", s.handleJobDetail)
	group.GET("/runs", s.handleRunHistory)
	group.GET("/runs/:run", s.handleRun)
	group.GET("/runs/:run/events", s.handleRunEvents)
	group.GET("/nodes/:hash", s.handleNode)
	group.GET("/config/:sha", s.handleConfig)
	group.GET("/approvals", s.handleApprovals)
	group.GET("/questions", s.handleQuestions)
	group.GET("/mcp", s.handleMCP)
	group.GET("/resources", s.handleResources)
	group.GET("/resources/:resource", s.handleResource)
	group.GET("/search", s.handleSearch)
	group.GET("/jobs/:job/follow", s.handleFollow)
	group.GET("/jobs/:job/latest-run", s.handleLatestRun)

	group.POST("/jobs/:job/trigger", s.handleTrigger)
	group.POST("/approvals/:id", s.handleDecideApproval)
	group.POST("/questions/:id", s.handleAnswerQuestion)
	group.POST("/jobs/:job/resume", s.handleResumeBreaker)
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

// resolvePipeline is the middleware that turns :pipeline into the loaded
// pipeline every handler under /p/ works from.
func (s *Server) resolvePipeline(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		pipeline := s.Lookup(c.Param("pipeline"))
		if pipeline == nil {
			return echo.NewHTTPError(http.StatusNotFound, "no such pipeline")
		}

		c.Set("pipeline", pipeline)

		return next(c)
	}
}

// pipelineOf returns the pipeline resolved for this request.
func pipelineOf(c *echo.Context) *Pipeline {
	pipeline, _ := c.Get("pipeline").(*Pipeline)

	return pipeline
}

// sameOriginMutations rejects a state-changing request whose Origin is not
// the host it was sent to. Safe methods pass untouched, and a request with no
// Origin header at all passes too — that is a curl or a CLI, not a browser
// being aimed at this port by another page.
func sameOriginMutations(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		method := c.Request().Method
		if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
			return next(c)
		}

		// A webhook is authenticated by its signature and sent by a machine that
		// has no reason to share this origin. Exempting it here rather than
		// mounting it outside the group keeps it under /p/<slug>/, which is
		// what says which pipeline it checks.
		if strings.HasSuffix(c.Path(), "/hooks/:resource") {
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

// No page here calls /api, and to the origin check a DNS-rebinding page is same-origin (its own name arrives as both Host and Origin) while a set is a remote shell; not a Host check, because a reverse proxy forwards the client's.
func refuseBrowsers(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		header := c.Request().Header
		site := header.Get("Sec-Fetch-Site")

		// Origin is the half that holds: every browser sends it on a PUT, POST or DELETE, while Sec-Fetch-Site reaches only https and loopback URLs, which a rebinding page is not.
		if header.Get("Origin") != "" || (site != "" && site != "none") {
			return echo.NewHTTPError(http.StatusForbidden, "the /api routes answer the steps CLI, not a browser")
		}

		// ponytail: a rebinding page's GET carries neither and is answered, showing it nothing the UI's own pages do not; an opt-in Host allow-list would close both.
		return next(c)
	}
}

// handleError renders a failure as a page rather than echo's JSON default —
// this server only ever talks to a browser.
func (s *Server) handleError(c *echo.Context, err error) {
	if response, _ := echo.UnwrapResponse(c.Response()); response != nil && response.Committed {
		return
	}

	status := http.StatusInternalServerError
	message := err.Error()

	var httpErr *echo.HTTPError
	if errors.As(err, &httpErr) {
		status = httpErr.Code
		message = httpErr.Message
	} else if code := echo.StatusCode(err); code != 0 {
		// echo's own status sentinels carry a code and nothing else — an oversized `steps pipeline set` returns ErrStatusRequestEntityTooLarge, which errors.As does not see, and a 413 reported as 500 tells the terminal the daemon broke rather than that its upload was too big.
		status = code
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
func (s *Server) globalNav(c *echo.Context) navData {
	nav, gathered := s.navGathered(c)

	if nav.Current == "" && len(nav.Pipelines) > 0 {
		nav.Current = nav.Pipelines[0].Slug
		nav.CurrentPath = nav.Pipelines[0].Path
		// The shell's badges have to belong to the pipeline its tabs point at, or /docs reports a clean pipeline's counts on links into a different one. Anchored stays false: the badges are chrome pointing into that pipeline, while the LIST says "this pipeline", which on a page listing several would name none of them.
		nav.Attention = gathered[nav.Current]
		// The tabs this shell draws have to be the ones that pipeline HAS, or /docs and the overview offer an mcp tab that 404s on the pipeline they anchor to.
		nav.HasMCP = s.hasMCP(nav.Current)
	}

	return nav
}

// hasMCP answers the tab question for a pipeline that is not the request's own — the shell an overview or a docs page draws is anchored to one it did not resolve.
func (s *Server) hasMCP(slug string) bool {
	pipeline := s.Lookup(slug)

	return pipeline != nil && len(pipeline.Config().MCPServers) > 0
}

func (s *Server) nav(c *echo.Context) navData {
	nav, _ := s.navGathered(c)

	return nav
}

// navGathered is nav plus the per-pipeline items it read on the way, so the
// pages that anchor themselves to a pipeline they did not resolve do not ask
// the same databases the same questions twice in one render.
func (s *Server) navGathered(c *echo.Context) (navData, map[string][]attentionItem) {
	ctx := c.Request().Context()
	nav := navData{ReadOnly: s.runner == nil, URL: c.Request().URL.RequestURI()}

	// Every served pipeline, not only the request's own: a reader looking at a
	// clean pipeline has no other way to learn that one of the daemon's others
	// has stopped, short of opening it.
	//
	// ponytail: recomputed per request, so a daemon holding many pipelines
	// pays for all of them on every 2.5s self-poll of every open tab. The
	// store reads are small and indexed, and the one that was NOT — what a
	// saved oauth token is worth, which is a file — is cached against that
	// file by whoever holds it (internal/cli's credentials). Give this a
	// short TTL if it ever shows up in a profile.
	served := s.Served()
	gathered := make(map[string][]attentionItem, len(served))

	for _, pipeline := range served {
		items := s.attention(ctx, pipeline)
		gathered[pipeline.Slug] = items

		nav.Pipelines = append(nav.Pipelines, pipelineSummary{
			Slug:      pipeline.Slug,
			Path:      pipeline.Path(),
			Jobs:      len(pipeline.Config().Jobs),
			Attention: attentionTotal(items),
		})
	}

	sort.Slice(nav.Pipelines, func(i, j int) bool {
		return nav.Pipelines[i].Slug < nav.Pipelines[j].Slug
	})

	current := pipelineOf(c)
	if current == nil {
		return nav, gathered
	}

	nav.Current = current.Slug
	nav.CurrentPath = current.Path()
	nav.Anchored = true
	nav.Attention = gathered[current.Slug]
	// The tab appears only for a pipeline that declares servers: most do not, and a dead tab on every one of them is nav space spent on a feature they never use.
	nav.HasMCP = len(current.Config().MCPServers) > 0

	return nav, gathered
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
	URL string
	// Attention is what this pipeline is waiting on a person for, most
	// blocking first — see attention.go for what is allowed in it.
	Attention []attentionItem
	// Anchored is whether Current is the pipeline this request actually
	// resolved, rather than the first one a page above /p/:pipeline borrowed
	// to keep its links alive. The attention LIST says "this pipeline" and
	// carries that pipeline's Unpause button, so the root and /docs — which
	// sit over several — draw it only when it is theirs to draw.
	Anchored bool
	ReadOnly bool
	// HasMCP is whether the current pipeline declares mcp_servers:, which is the only thing that draws the mcp tab.
	HasMCP bool
}

// Paused reports whether the current pipeline is paused, which two pages ask
// about for reasons the banner does not cover: the jobs board offers a Pause
// button only when there is something to pause.
func (n navData) Paused() bool {
	for _, item := range n.Attention {
		if item.Kind == "paused" {
			return true
		}
	}

	return false
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
	// Attention is everything that pipeline is waiting on, summed — a switcher row has space for a number, not for six sentences.
	Attention int
}
