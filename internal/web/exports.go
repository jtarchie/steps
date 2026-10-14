package web

// Serving a pipeline's MCP exports to agents that connected through
// oauthserver.go. Every request carries an access token; the token names the
// grant — which person let which client use which export — and nothing
// passes unless that grant is for the export this path names and the person
// is still allowed it. Every call is recorded under that person's name.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/internal/actor"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/scheduler"
	"github.com/jtarchie/steps/internal/store"
)

// Exporter builds a pipeline's exports; scheduler.LocalRunner is one. Absent on a read-only server, which serves no exports: calling a tool is acting, the thing read-only withholds.
type Exporter interface {
	ServeExport(ctx context.Context, target *scheduler.Pipeline, export config.MCPExport, hooks scheduler.ExportHooks) (*scheduler.ExportServer, error)
}

// WithExporter lets the server serve exports, built by exporter.
func WithExporter(exporter Exporter) Option {
	return func(s *Server) { s.exporter = exporter }
}

// servedExport is one export built under one configuration; a set that changes the configuration retires it.
type servedExport struct {
	cfg    *config.Config
	server *scheduler.ExportServer
}

type exportCache struct {
	mu      sync.Mutex
	entries map[string]servedExport
	// calls counts each person's calls per export inside the current rate window.
	calls map[string][]time.Time
}

type grantKey struct{}

// handleExport answers an agent's MCP request to an export.
func (s *Server) handleExport(c *echo.Context) error {
	ctx := c.Request().Context()

	grant, ok := s.bearerGrant(c)
	if !ok {
		c.Response().Header().Set(echo.HeaderWWWAuthenticate, `Bearer resource_metadata="`+s.external+`/.well-known/oauth-protected-resource/mcp/x/`+c.Param("pipeline")+`/`+c.Param("export")+`"`)

		return echo.ErrUnauthorized
	}

	target, export, found := s.exportNamed(grant.Pipeline, grant.Export)
	if !found || s.exporter == nil {
		return echo.NewHTTPError(http.StatusNotFound, "no such export")
	}

	who := actor.Parse(grant.Actor)

	// Asked on every request, not only at a refresh: a team someone left, or an export a set narrowed, is known here before the token expires.
	id, allowed := s.mayUseAs(ctx, who, target, export)
	if !allowed {
		return echo.NewHTTPError(http.StatusForbidden, "access to this export has ended")
	}

	served, err := s.exportFor(target, export)
	if err != nil {
		slog.ErrorContext(ctx, "web.export_unbuilt", "pipeline", target.Slug, "export", export.Name, "error", err)

		return echo.NewHTTPError(http.StatusBadGateway, "this export could not reach its tools")
	}

	// The rules on the export's tools are written against this person, as the access: rule just was.
	called := scheduler.WithExportWho(context.WithValue(actor.With(ctx, who), grantKey{}, grant), scheduler.ExportWho{
		Provider: id.Provider, UserID: id.UserID, ID: id.ID, Email: id.Email, Organizations: id.Organizations, Groups: id.Groups,
	})

	req := c.Request().WithContext(called)
	served.Handler.ServeHTTP(c.Response(), req)

	return nil
}

// bearerGrant is the grant the request's access token was issued under, when that is a live access token for the export this path names. A token for one export is no use at another.
func (s *Server) bearerGrant(c *echo.Context) (store.Grant, bool) {
	raw, found := strings.CutPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Bearer ")
	if !found || raw == "" || s.sessions == nil {
		return store.Grant{}, false
	}

	token, live, err := s.sessions.FindToken(c.Request().Context(), tokenDigest(raw))
	if err != nil || !live || token.Kind != "access" {
		return store.Grant{}, false
	}

	if token.Grant.Pipeline != c.Param("pipeline") || token.Grant.Export != c.Param("export") {
		return store.Grant{}, false
	}

	return token.Grant, true
}

// exportFor is the export built under target's current configuration, building it on first use and retiring the one a set replaced. The upstream connections it holds outlive any one request, so they live under the server's own context.
func (s *Server) exportFor(target *Pipeline, export config.MCPExport) (*scheduler.ExportServer, error) {
	key := target.Slug + "/" + export.Name
	cfg := target.Config()

	if server, built := s.builtExport(key, cfg); built {
		return server, nil
	}

	// Built outside the lock: building dials every upstream the export reaches, and every other export's calls take this lock for their rate.
	server, err := s.exporter.ServeExport(context.WithoutCancel(context.Background()), target.Pipeline, export, s.exportHooks(target, export))
	if err != nil {
		return nil, err //nolint:wrapcheck // the export names itself
	}

	s.exports.mu.Lock()
	defer s.exports.mu.Unlock()

	if current, built := s.exports.entries[key]; built {
		if current.cfg == cfg {
			_ = server.Close()

			return current.server, nil
		}

		_ = current.server.Close()
	}

	if s.exports.entries == nil {
		s.exports.entries = map[string]servedExport{}
	}

	s.exports.entries[key] = servedExport{cfg: cfg, server: server}

	return server, nil
}

func (s *Server) builtExport(key string, cfg *config.Config) (*scheduler.ExportServer, bool) {
	s.exports.mu.Lock()
	defer s.exports.mu.Unlock()

	current, built := s.exports.entries[key]

	return current.server, built && current.cfg == cfg
}

func (s *Server) exportHooks(target *Pipeline, export config.MCPExport) scheduler.ExportHooks {
	return scheduler.ExportHooks{
		Before: func(ctx context.Context, _ string) error {
			return s.allowCall(ctx, export)
		},
		After: func(ctx context.Context, tool string, argNames []string, ok bool, rule string) {
			grant, _ := ctx.Value(grantKey{}).(store.Grant)

			// A call a rule refused is its own verb, so the log and the export's page say which rule and its count is not the calls that ran; only the rule's name is kept, like argument names, because everything else about it is what a caller pasted.
			action, detail := "mcp call", map[string]any{"tool": tool, "args": argNames, "ok": ok}
			if rule != "" {
				action, detail = "mcp refused", map[string]any{"tool": tool, "rule": rule, "reason": "refused by rule " + rule}
			}

			encoded, err := json.Marshal(detail)
			if err != nil {
				return
			}

			s.recordGrantActivity(ctx, target, grant, action, string(encoded))
		},
	}
}

// allowCall applies an export's rate: to the person calling, counting only calls inside the window.
func (s *Server) allowCall(ctx context.Context, export config.MCPExport) error {
	if export.Rate == "" {
		return nil
	}

	limit, window, err := config.ParseRate(export.Rate)
	if err != nil {
		// A set refuses a rate that does not parse, so one here is from before and counts as none.
		return nil
	}

	grant, _ := ctx.Value(grantKey{}).(store.Grant)
	key := grant.Actor + "\x00" + grant.Pipeline + "\x00" + grant.Export
	now := time.Now()

	s.exports.mu.Lock()
	defer s.exports.mu.Unlock()

	if s.exports.calls == nil {
		s.exports.calls = map[string][]time.Time{}
	}

	recent := s.exports.calls[key][:0]
	for _, at := range s.exports.calls[key] {
		if now.Sub(at) < window {
			recent = append(recent, at)
		}
	}

	if len(recent) >= limit {
		s.exports.calls[key] = recent

		return scheduler.ErrRateLimited
	}

	s.exports.calls[key] = append(recent, now)

	return nil
}

// Close lets go of every export's upstream connections. Pages need nothing closed; an export holds a live MCP connection per server it reaches.
func (s *Server) Close() error {
	s.exports.mu.Lock()
	defer s.exports.mu.Unlock()

	errs := make([]error, 0, len(s.exports.entries))

	for key, served := range s.exports.entries {
		errs = append(errs, served.server.Close())
		delete(s.exports.entries, key)
	}

	return errors.Join(errs...)
}
