package resource

import (
	"context"
	"sync"

	"github.com/jtarchie/steps/internal/config"
)

// Cache memoizes ResolveVersions results within one RunJob invocation, keyed
// by resource name (step.Get). A get step's resolved versions are a pure
// function of (cfg, step, pinned) — none of which change between the
// plan-time call (merkle.PlanChains) and the run-time call (the pipeline's
// runGetStep) within one invocation — so reusing the plan-time result for
// the run-time call is exact, not approximate, and avoids paying for the
// check command (and, for an imaged resource type, a docker container)
// twice per get step per run. A nil *Cache is a valid, always-miss receiver
// (see ResolveVersionsCached), so callers with no cross-phase reuse to offer
// (most tests) can simply not construct one.
type Cache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	// consumed reports whether this job has already fanned out over a version
	// — see WithConsumed. Nil means no filtering, which is every caller that
	// has no store to ask.
	consumed func(resourceName string, version map[string]any) bool
}

// CacheOption configures a Cache at construction.
type CacheOption func(*Cache)

// WithConsumed filters a `version: every` fan-out down to the versions this
// job has NOT already taken.
//
// It belongs on the Cache rather than inside ResolveVersions because the Cache
// is the single point both the plan-time caller (merkle.PlanChains) and the
// run-time one (the pipeline's runGetStep) go through: filtering here means
// the two cannot see different lists, which is the invariant that keeps a
// plan's hashes describing the run that follows. It is also what keeps this
// package free of any dependency on the store — the caller supplies the
// answer, this package only applies it.
//
// Only `version: every` is filtered. Every other mode narrows to a single
// version whose repetition is already governed by the merkle cache.
func WithConsumed(consumed func(resourceName string, version map[string]any) bool) CacheOption {
	return func(c *Cache) { c.consumed = consumed }
}

type cacheEntry struct {
	resource     *config.Resource
	resourceType *config.ResourceType
	versions     []map[string]any
	err          error
}

// NewCache returns an empty Cache, scoped to one RunJob invocation. Never
// share one Cache instance across concurrent RunJob calls (as steps watch
// --max-concurrent > 1 makes possible) — each invocation must get its own,
// since a cached miss vs. hit is only valid within the single (cfg, pinned)
// combination one invocation resolves against.
func NewCache(opts ...CacheOption) *Cache {
	cache := &Cache{entries: map[string]cacheEntry{}}

	for _, opt := range opts {
		opt(cache)
	}

	return cache
}

// ResolveVersionsCached behaves exactly like ResolveVersions, but returns a
// memoized result for a resource name already resolved earlier through this
// same cache. A nil c always misses, equivalent to calling ResolveVersions
// directly.
func (c *Cache) ResolveVersionsCached(
	ctx context.Context, cfg *config.Config, step config.Step, pinned map[string]string,
) (*config.Resource, *config.ResourceType, []map[string]any, error) {
	if c == nil {
		return ResolveVersions(ctx, cfg, step, pinned)
	}

	c.mu.Lock()
	entry, ok := c.entries[step.Get]
	c.mu.Unlock()

	if ok {
		return entry.resource, entry.resourceType, entry.versions, entry.err
	}

	resource, resourceType, versions, err := ResolveVersions(ctx, cfg, step, pinned)
	if err == nil {
		versions = c.unconsumed(step, pinned, resource, versions)
	}

	c.mu.Lock()
	c.entries[step.Get] = cacheEntry{resource, resourceType, versions, err}
	c.mu.Unlock()

	return resource, resourceType, versions, err
}

// unconsumed drops the versions this job has already fanned out over, and only
// for the one mode that fans out. A CLI --version pin beats version: every in
// ResolveVersions, so a pinned run is left alone here too: naming a version
// explicitly is an instruction, not a question about what is new.
func (c *Cache) unconsumed(
	step config.Step, pinned map[string]string, resource *config.Resource, versions []map[string]any,
) []map[string]any {
	if c.consumed == nil || len(pinned) > 0 || resource == nil {
		return versions
	}

	if mode, _ := VersionMode(step); mode != "every" {
		return versions
	}

	fresh := make([]map[string]any, 0, len(versions))

	for _, version := range versions {
		if c.consumed(resource.Name, version) {
			continue
		}

		fresh = append(fresh, version)
	}

	return fresh
}
