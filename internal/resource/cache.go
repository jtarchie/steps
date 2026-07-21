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
func NewCache() *Cache {
	return &Cache{entries: map[string]cacheEntry{}}
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

	c.mu.Lock()
	c.entries[step.Get] = cacheEntry{resource, resourceType, versions, err}
	c.mu.Unlock()

	return resource, resourceType, versions, err
}
