package web

import (
	"sort"

	"github.com/jtarchie/steps/internal/actor"
	"github.com/labstack/echo/v5"
)

// globalNav is nav() for a page that has no current pipeline: the overview,
// /docs and an error page all sit above `/p/:pipeline`, so pipelineOf is nil
// there. The shell draws no section tabs for such a page — tabs borrowed from
// the first pipeline looked like the root's own sections, and "jobs" clicked
// from / landed on a pipeline nobody chose — but the error page's way back
// and the jump palette's search URL still need a pipeline to point at, and
// rendered raw they emit "/p//" links that 404.
//
// Anchored stays false: the switcher reads as a picker, the attention list
// (which says "this pipeline") is not drawn, and nothing on the page is
// scoped by the borrowed name.
func (s *Server) globalNav(c *echo.Context) navData {
	nav := s.nav(c)

	for _, first := range nav.Pipelines {
		// A broken pipeline has no pages to borrow links from.
		if nav.Current != "" || first.Broken {
			continue
		}

		nav.Current = first.Slug
		nav.CurrentPath = first.Path
	}

	return nav
}

// unscopedNav is globalNav for a page about no pipeline at all — the docs,
// an error — whose tab says nothing rather than borrowing every pipeline's.
func (s *Server) unscopedNav(c *echo.Context) navData {
	nav := s.globalNav(c)
	nav.Mark = mark{}
	nav.MarkURL = ""

	return nav
}

// nav is the shell every page renders inside: which pipelines exist, which
// one is current, and the counts the top bar reports.
func (s *Server) nav(c *echo.Context) navData {
	ctx := c.Request().Context()
	nav := navData{ReadOnly: s.runner == nil, URL: c.Request().URL.RequestURI(), Role: RoleAdmin}

	s.signedInNav(c, &nav)

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
	served := s.servedTo(c)
	gathered := make(map[string][]attentionItem, len(served))

	for _, pipeline := range served {
		reads := readsOf(c, pipeline)
		items := s.attention(ctx, reads)
		gathered[pipeline.Slug] = items

		nav.Pipelines = append(nav.Pipelines, pipelineSummary{
			Slug:      pipeline.Slug,
			Path:      pipeline.Path(),
			Jobs:      len(pipeline.Config().Jobs),
			Attention: attentionTotal(items),
			Mark:      pipelineMark(ctx, reads, attentionTotal(items)),
			InFlight:  inFlight(ctx, reads),
		})
	}

	for _, broken := range s.brokenTo(c) {
		nav.Pipelines = append(nav.Pipelines, pipelineSummary{Slug: broken.Name, Path: broken.From, Broken: true, Mark: mark{Disc: discErrored}})
	}

	// Above a pipeline the page is about all of them; a pipeline page
	// replaces this with its own below.
	for _, summary := range nav.Pipelines {
		nav.Mark = nav.Mark.fold(summary.Mark)
	}

	nav.MarkURL = "/mark"
	nav.SwitchMark = nav.Mark

	sort.Slice(nav.Pipelines, func(i, j int) bool {
		return nav.Pipelines[i].Slug < nav.Pipelines[j].Slug
	})

	current := pipelineOf(c)
	if current == nil {
		return nav
	}

	nav.Current = current.Slug
	nav.CurrentPath = current.Path()
	nav.Anchored = true
	nav.Mark = mark{}
	nav.MarkURL = "/p/" + current.Slug + "/mark"

	for _, summary := range nav.Pipelines {
		if summary.Slug == current.Slug {
			nav.Mark = summary.Mark
			nav.SwitchMark = summary.Mark
		}
	}
	nav.Paused = readsOf(c, current).Paused(ctx)
	nav.Attention = gathered[current.Slug]
	// The tab appears only for a pipeline that declares servers: most do not, and a dead tab on every one of them is nav space spent on a feature they never use.
	nav.HasMCP = len(current.Config().MCPServers) > 0 || len(current.Config().MCPExports) > 0

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
	// Paused is whether the current pipeline is paused, which the action bar says on every page.
	Paused bool
	// Mark is what this page's title and favicon say: the narrowest run or
	// pipeline the page is about, every pipeline above one, zero on a page
	// that is about none (docs, an error).
	Mark mark
	// MarkURL is where a hidden tab asks for Mark again; empty on a page
	// about nothing, which then never polls.
	MarkURL string
	// SwitchMark is what the switcher button wears: the current pipeline's
	// mark even on a run page, and every pipeline's above one — the switcher
	// is about pipelines whatever page it sits on.
	SwitchMark mark
	// Who is the signed-in person's stored actor, which draws the user menu; empty on a server nobody signs in to, where there is nobody to name.
	Who string
	// CanSignOut is a browser that signed in through the form; one the Basic prompt let in has nothing the daemon can end.
	CanSignOut bool
	// Role is what the person reading may do. ViewOnly is a server with controls whose reader may not use them, which ReadOnly then also says, so every page that hides controls for one hides them for the other; the two differ only in the sentence explaining why.
	Role     Role
	ViewOnly bool
}

type pipelineSummary struct {
	Slug string
	Path string
	Jobs int
	// Attention is everything that pipeline is waiting on, summed — a switcher row has space for a number, not for six sentences.
	Attention int
	// Broken is a pipeline the daemon holds and does not serve. Not its reason: this shell also draws the error page a webhook sender gets, and that sender never passed this server's credentials.
	Broken bool
	Mark   mark
	// InFlight is the trigger queue's open rows, queued and running: every
	// build the daemon starts is claimed from it. Activity, not attention, so
	// it is drawn dim and only when nonzero.
	InFlight int
}

// signedInNav fills in who is reading and what they may do, on a server people sign in to.
func (s *Server) signedInNav(c *echo.Context, nav *navData) {
	if s.auth == nil {
		return
	}

	nav.Who = actor.From(c.Request().Context()).String()
	nav.CanSignOut = s.hasSession(c)
	nav.Role = s.roleOf(c)
	nav.ViewOnly = s.runner != nil && nav.Role < RoleOperator
	nav.ReadOnly = nav.ReadOnly || nav.ViewOnly
}
