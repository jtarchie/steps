package web

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/labstack/echo/v5"
)

// Add starts serving a pipeline, refusing a name already held.
func (s *Server) Add(pipeline *Pipeline) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, clash := s.bySlug[pipeline.Slug]; clash {
		return fmt.Errorf("web: this daemon already serves a pipeline called %q", pipeline.Slug)
	}

	s.bySlug[pipeline.Slug] = pipeline
	s.pipelines = append(s.pipelines, pipeline)
	// Served is repaired: a set that got this far passed every check the restart failed.
	delete(s.broken, pipeline.Slug)

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

// BrokenPipeline is one the database holds that this daemon could not serve, and why.
type BrokenPipeline struct {
	Name   string
	SHA    string
	From   string
	Reason string
}

// brokenReasonLimit bounds what a page and a JSON row carry: a problem list is a few lines, and one pathological error must not become a megabyte on every page's switcher.
const brokenReasonLimit = 4 << 10

// MarkBroken lists a pipeline as held and not served.
func (s *Server) MarkBroken(broken BrokenPipeline) {
	if len(broken.Reason) > brokenReasonLimit {
		broken.Reason = strings.ToValidUTF8(broken.Reason[:brokenReasonLimit], "") + "…"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.broken[broken.Name] = broken
}

// ClearBroken forgets a broken pipeline, once nothing holds it any more.
func (s *Server) ClearBroken(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.broken, name)
}

// Broken is every pipeline held and not served, by name.
func (s *Server) Broken() []BrokenPipeline {
	s.mu.RLock()
	defer s.mu.RUnlock()

	broken := slices.Collect(maps.Values(s.broken))
	slices.SortFunc(broken, func(a, b BrokenPipeline) int { return strings.Compare(a.Name, b.Name) })

	return broken
}

// BrokenReason is why a pipeline is held and not served, and false when it is not broken.
func (s *Server) BrokenReason(name string) (string, bool) {
	broken, ok := s.brokenOf(name)

	return broken.Reason, ok
}

func (s *Server) brokenOf(name string) (BrokenPipeline, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	broken, ok := s.broken[name]

	return broken, ok
}

// notServed is the 404 for a name this daemon does not serve, which says why when the name is one it holds broken.
func (s *Server) notServed(name string) error {
	if reason, broken := s.BrokenReason(name); broken {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("%s is not being served: %s", name, reason))
	}

	return echo.NewHTTPError(http.StatusNotFound, ErrNoSuchPipeline.Error())
}
