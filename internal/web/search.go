package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/labstack/echo/v5"
)

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
func (s *Server) handleSearch(c *echo.Context) error {
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
			Hint:  other.Path(),
			URL:   "/p/" + other.Slug,
			match: "pipeline " + other.Slug + " " + other.Path(),
		})
	}

	for _, other := range order {
		addJobHits(add, other, other != pipeline)
	}

	for _, other := range order {
		if full() {
			// Nothing left to fill, and a run query is the expensive part:
			// the palette refetches on every keystroke, and under one shared
			// --db file these serialize on the same connection the runner
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
	served := s.Served()

	out := make([]*Pipeline, 0, len(served))

	for _, pipeline := range served {
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

	for _, job := range pipeline.Config().Jobs {
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
		slog.WarnContext(ctx, "web.search.runs_unavailable", "pipeline", pipeline.Slug, "error", err)

		return
	}

	for _, run := range runs {
		hint := string(run.Status)
		if elsewhere {
			hint = pipeline.Slug + " " + hint
		}

		add(searchHit{
			Kind:  "run",
			Name:  run.JobName + " " + shortID(run.ID),
			Hint:  hint,
			URL:   fmt.Sprintf("/p/%s/runs/%s", pipeline.Slug, run.ID),
			match: "run " + run.JobName + " " + run.ID + " " + string(run.Status) + " " + pipeline.Slug,
		})
	}
}
