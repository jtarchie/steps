package web

// The activity log: what people did, recorded in one place as each mutating
// route succeeds, rather than by each handler remembering to.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/internal/actor"
	"github.com/jtarchie/steps/internal/store"
)

// activityRoute is how one mutating route reads as an entry.
type activityRoute struct {
	action func(c *echo.Context) string
	target func(c *echo.Context, p *Pipeline) string
	detail func(c *echo.Context) map[string]string
	// role is what the route asks of the person calling it.
	role Role
	// before records ahead of the handler, for the one verb that leaves no handle to record through afterwards. A destroy that then fails leaves an entry for an attempt; the handler refuses an unknown pipeline before that can matter.
	before bool
}

func verb(name string) func(*echo.Context) string { return func(*echo.Context) string { return name } }

func param(name string) func(*echo.Context, *Pipeline) string {
	return func(c *echo.Context, _ *Pipeline) string { return c.Param(name) }
}

func numbered(noun, name string) func(*echo.Context, *Pipeline) string {
	return func(c *echo.Context, _ *Pipeline) string { return noun + " " + c.Param(name) }
}

func nothing(*echo.Context, *Pipeline) string { return "" }

// reasonDetail keeps what a person typed beside a decision, bounded: it is a note, and the approval row holds the whole of it.
func reasonDetail(c *echo.Context) map[string]string {
	reason := c.FormValue("reason")
	if reason == "" {
		return nil
	}

	const maxReason = 200
	if len(reason) > maxReason {
		reason = reason[:maxReason] + "…"
	}

	return map[string]string{"reason": reason}
}

func currentSHA(c *echo.Context, p *Pipeline) string {
	revision, found, err := p.Store.CurrentRevision(c.Request().Context())
	if err != nil || !found {
		return ""
	}

	return revision.SHA
}

// activityRoutes is every mutating route a person uses, by method and path. TestEveryMutatingRouteIsRecordedOrExcused holds it to the router, so a new one cannot ship unrecorded by accident.
var activityRoutes = map[string]activityRoute{
	"POST /p/:pipeline/jobs/:job/trigger": {role: RoleOperator, action: verb("trigger"), target: param("job"), detail: func(c *echo.Context) map[string]string {
		if c.FormValue("force") == forceAll {
			return map[string]string{"cache": "skipped"}
		}

		return nil
	}},
	"POST /p/:pipeline/runs/:run/rerun": {role: RoleOperator, action: verb("rerun"), target: param("run")},
	"POST /p/:pipeline/approvals/:id": {role: RoleOperator, action: func(c *echo.Context) string {
		if c.FormValue("decision") == "reject" {
			return "reject"
		}

		return "approve"
	}, target: numbered("approval", "id"), detail: reasonDetail},
	"POST /p/:pipeline/questions/:id":                      {role: RoleOperator, action: verb("answer"), target: numbered("question", "id")},
	"POST /p/:pipeline/jobs/:job/release":                  {role: RoleOperator, action: verb("release"), target: param("job")},
	"POST /p/:pipeline/pause":                              {role: RoleOperator, action: verb("pause"), target: nothing},
	"POST /p/:pipeline/unpause":                            {role: RoleOperator, action: verb("unpause"), target: nothing},
	"POST /p/:pipeline/runs/:run/abort":                    {role: RoleOperator, action: verb("abort"), target: param("run")},
	"POST /p/:pipeline/jobs/:job/queued/abort":             {role: RoleOperator, action: verb("abort queued"), target: param("job")},
	"POST /p/:pipeline/mcp/:server/connect":                {role: RoleAdmin, action: verb("mcp connect"), target: param("server")},
	"POST /p/:pipeline/mcp/:server/test":                   {role: RoleAdmin, action: verb("mcp test"), target: param("server")},
	"PUT /api/pipelines/:pipeline":                         {role: RoleAdmin, action: verb("set"), target: currentSHA},
	"DELETE /api/pipelines/:pipeline":                      {role: RoleAdmin, action: verb("destroy"), target: nothing, before: true},
	"POST /api/pipelines/:pipeline/pause":                  {role: RoleOperator, action: verb("pause"), target: nothing},
	"POST /api/pipelines/:pipeline/unpause":                {role: RoleOperator, action: verb("unpause"), target: nothing},
	"POST /api/pipelines/:pipeline/rename":                 {role: RoleAdmin, action: verb("rename"), target: param("pipeline")},
	"POST /api/pipelines/:pipeline/runs/:run/abort":        {role: RoleOperator, action: verb("abort"), target: param("run")},
	"POST /api/pipelines/:pipeline/jobs/:job/queued/abort": {role: RoleOperator, action: verb("abort queued"), target: param("job")},
	"POST /api/pipelines/:pipeline/mcp/:server/login":      {role: RoleAdmin, action: verb("mcp connect"), target: param("server")},
}

// notRecorded are the mutating routes a person does not drive, with why.
var notRecorded = map[string]string{ //nolint:gosec // route keys and reasons, not credentials
	"POST /login":                       "signing in is about the daemon, not a pipeline, and the log is written through a pipeline's handle; the daemon's log line web.signed_in records it",
	"POST /logout":                      "as POST /login: web.signed_out in the daemon's log",
	"POST /oauth/register":              "an agent registering itself is nobody acting; the person who later lets it connect is recorded then",
	"POST /oauth/token":                 "an agent exchanging what a person already granted; the grant was recorded when it was given",
	"POST /oauth/authorize":             "recorded by handleConsent itself, which knows the export the consent named — this route carries no pipeline in its path",
	"POST /p/:pipeline/hooks/:resource": "a webhook delivery is automatic: the run it starts already says webhook: <resource>",
}

// recordActivity writes an entry for a mutating route once it has succeeded, through the handle of the pipeline it acted on.
func (s *Server) recordActivity(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		route, recorded := activityRoutes[c.Request().Method+" "+c.Path()]
		if !recorded {
			return next(c)
		}

		name := c.Param("pipeline")
		target := s.Lookup(name)

		if route.before {
			if target != nil {
				s.writeActivity(c, target, route)
			}

			return next(c)
		}

		err := next(c)

		_, status := echo.ResolveResponseStatus(c.Response(), err)
		if status >= http.StatusBadRequest {
			return err
		}

		// A set may have just created the pipeline it names.
		if target == nil {
			target = s.Lookup(name)
		}

		if target != nil {
			s.writeActivity(c, target, route)
		}

		return err
	}
}

// writeActivity never fails the request: the act already happened, and refusing to say so would be worse than a missing line, which is logged.
func (s *Server) writeActivity(c *echo.Context, p *Pipeline, route activityRoute) {
	entry := store.ActivityEntry{
		Actor:  actor.From(c.Request().Context()).String(),
		Action: route.action(c),
		Target: route.target(c, p),
	}

	if route.detail != nil {
		if detail := route.detail(c); len(detail) > 0 {
			encoded, err := json.Marshal(detail)
			if err == nil {
				entry.Detail = string(encoded)
			}
		}
	}

	// Under its own context: the act succeeded, and a client that hung up on the redirect must not take the record of it along.
	err := p.Store.RecordActivity(context.WithoutCancel(c.Request().Context()), entry, s.activityCap())
	if err != nil {
		slog.ErrorContext(c.Request().Context(), "web.activity_unrecorded", "pipeline", p.Slug, "action", entry.Action, "error", err)
	}
}

func (s *Server) activityCap() int {
	if s.activity.configured {
		return s.activity.cap
	}

	return store.DefaultActivityCap
}

// activityLog is the daemon-wide log's settings: how to read it when no pipeline is served to borrow a reader from, and how much of it to keep.
type activityLog struct {
	open       func() (store.Reader, error)
	cap        int
	configured bool
}

// WithActivityLog says how to open the database the log lives in, for when every pipeline is gone and the log still holds what was done to them, and how many entries it keeps (zero is the default).
func WithActivityLog(open func() (store.Reader, error), limit int) Option {
	return func(s *Server) { s.activity = activityLog{open: open, cap: limit, configured: true} }
}

// activityReader borrows a served pipeline's reader — one database holds them all — and opens one of its own only when nothing is served; release closes only what it opened.
func (s *Server) activityReader() (store.Reader, func()) {
	for _, p := range s.Served() {
		return p.Store.Reader(), func() {}
	}

	if s.activity.open == nil {
		return nil, func() {}
	}

	reader, err := s.activity.open()
	if err != nil {
		return nil, func() {}
	}

	return reader, func() { _ = reader.Close() }
}

const activityPage = 50

// activityRow is one entry as the page draws it.
type activityRow struct {
	store.ActivityEntry

	Reason string
	// Live is whether the pipeline still exists to link to.
	Live bool
	// TargetURL is where the target lives, empty when it has no page.
	TargetURL string
}

// activityFilter is one narrowing in force, and the URL without it.
type activityFilter struct {
	Label  string
	Value  string
	Remove string
}

func (s *Server) handleActivity(c *echo.Context) error {
	query := c.Request().URL.Query()
	filter := store.ActivityFilter{
		Pipeline: query.Get("pipeline"),
		Actor:    query.Get("actor"),
		Action:   query.Get("action"),
		Limit:    activityPage + 1,
	}

	filter.Before, _ = strconv.ParseInt(query.Get("before"), 10, 64)

	var entries []store.ActivityEntry

	reader, release := s.activityReader()
	defer release()

	if reader != nil {
		var err error

		entries, err = reader.Activity(c.Request().Context(), filter)
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}
	}

	older := ""
	if len(entries) > activityPage {
		entries = entries[:activityPage]
		older = withQuery(query, "before", strconv.FormatInt(entries[len(entries)-1].ID, 10))
	}

	rows := make([]activityRow, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, s.activityRowFor(entry))
	}

	var filters []activityFilter

	for _, f := range []struct{ key, label, value string }{
		{"pipeline", "pipeline", filter.Pipeline},
		{"actor", "person", actor.Parse(filter.Actor).Label()},
		{"action", "action", filter.Action},
	} {
		if query.Get(f.key) != "" {
			filters = append(filters, activityFilter{Label: f.label, Value: f.value, Remove: withQuery(query, f.key, "")})
		}
	}

	nav := s.globalNav(c)
	nav.Anchored = false

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "activity", map[string]any{
		"Nav":     nav,
		"Title":   "activity",
		"Section": "activity",
		"Rows":    rows,
		"Filters": filters,
		"Older":   older,
		"Paged":   filter.Before != 0,
		"Query":   query,
	})
}

func (s *Server) activityRowFor(entry store.ActivityEntry) activityRow {
	row := activityRow{ActivityEntry: entry, Live: s.Lookup(entry.Pipeline) != nil}

	var detail map[string]string
	if json.Unmarshal([]byte(entry.Detail), &detail) == nil {
		row.Reason = detail["reason"]
	}

	if !row.Live || entry.Target == "" {
		return row
	}

	base := "/p/" + url.PathEscape(entry.Pipeline)

	switch entry.Action {
	case "trigger", "release", "abort queued":
		row.TargetURL = base + "/jobs/" + url.PathEscape(entry.Target)
	case "rerun", "abort":
		row.TargetURL = base + "/runs/" + url.PathEscape(entry.Target)
	case "set":
		row.TargetURL = base + "/config/" + url.PathEscape(entry.Target)
	case "approve", "reject":
		row.TargetURL = base + "/approvals"
	case "answer":
		row.TargetURL = base + "/questions"
	case "mcp connect", "mcp test":
		row.TargetURL = base + "/mcp"
	}

	return row
}

// withQuery is the page's own URL with one parameter set, or removed when empty; paging always restarts, since an older-than cursor belongs to the filter it was read under.
func withQuery(query url.Values, key, value string) string {
	next := url.Values{}

	for k, v := range query {
		if k != key && k != "before" {
			next[k] = v
		}
	}

	if value != "" {
		next.Set(key, value)
	}

	if len(next) == 0 {
		return "/activity"
	}

	return "/activity?" + next.Encode()
}
