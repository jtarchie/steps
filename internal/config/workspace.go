package config

// The workspace: block: per-step filesystem isolation strategy and its
// strategy-specific options.

import (
	"errors"
	"fmt"
)

// WorkspaceConfig tunes the per-step workspace isolation every pipeline
// runs under: task/agent/put steps materialize a directory built from their
// own declared inputs:/outputs: (see Step, Task) — a step sees what it
// declared and nothing else. Isolation is not optional (there is no shared
// single-directory mode; an absent block simply takes every default here),
// the block only chooses HOW trees are materialized and where. This is
// corruption hygiene, not a security sandbox — a step's shell commands can
// still reach outside the materialized directory via absolute paths.
type WorkspaceConfig struct {
	// Strategy is "copy", the only one, kept so a pipeline spelling out the default still loads. It uses copy-on-write where the filesystem offers it (APFS clonefile on macOS, reflink on Linux) and a plain recursive copy otherwise.
	Strategy string `yaml:"strategy,omitempty"`
	// Root is where isolated build workspaces are materialized; the system temp directory when unset.
	Root string `yaml:"root,omitempty"`
	// Cache, when set, opts into the cross-build resource cache: a fetched
	// resource version is kept under Root and reused by later builds instead
	// of being re-fetched. Off by default, deliberately — see CacheConfig.
	Cache *CacheConfig `yaml:"cache,omitempty"`
}

// CacheConfig opts a pipeline into reusing fetched resource versions across
// builds, the disk-and-bandwidth win baggageclaim provides in Concourse.
//
// It is off by default because it changes an observable thing: a cached
// version's in: does NOT run again. That is correct under the resource
// contract — in: materializes a version, and the same version materializes
// the same content (see docs/conformance.md) — but a resource type whose in:
// has side effects beyond writing the directory (incrementing a counter,
// posting a notification) would see those stop happening. Opting in is the
// pipeline author asserting their in: is a pure fetch.
type CacheConfig struct {
	// Resources enables the cache. A separate field rather than the block's
	// presence meaning yes, so `cache: {resources: false}` can turn it off
	// without deleting tuning alongside it.
	Resources bool `yaml:"resources"`
	// MaxEntries bounds how many cached versions are kept, oldest-used
	// evicted first. 0 takes defaultCacheMaxEntries. A cache with no ceiling
	// grows until the disk does not, which on a long-lived daemon host is a
	// question of when, not whether.
	MaxEntries int `yaml:"max_entries,omitempty"`
}

// DefaultCacheMaxEntries bounds the resource cache when a pipeline enables it
// without saying how big. Sized as a judgment call — big enough that a
// pipeline with a handful of resources never evicts anything it still wants,
// small enough that an abandoned pipeline's cache is bounded.
const DefaultCacheMaxEntries = 50

// CacheEnabled reports whether the cross-build resource cache is on.
func (w *WorkspaceConfig) CacheEnabled() bool {
	return w != nil && w.Cache != nil && w.Cache.Resources
}

// CacheMaxEntries is the configured ceiling, or the default.
func (w *WorkspaceConfig) CacheMaxEntries() int {
	if w == nil || w.Cache == nil || w.Cache.MaxEntries <= 0 {
		return DefaultCacheMaxEntries
	}

	return w.Cache.MaxEntries
}

func (c *Config) validateWorkspace() error {
	ws := c.Workspace
	if ws == nil {
		return nil
	}

	switch ws.Strategy {
	case "", "copy":
	case "btrfs":
		return errors.New("workspace.strategy: btrfs was removed; omit strategy: — copy already uses copy-on-write where the filesystem offers it (reflink on btrfs and XFS, clonefile on APFS)")
	default:
		return fmt.Errorf("workspace.strategy %q must be copy", ws.Strategy)
	}

	return validateWorkspaceCache(ws)
}

// validateWorkspaceCache checks the cache: block. The root requirement is the
// substantive one: the cache outlives the run that filled it, so it cannot
// live in a directory the provider creates and removes — without an explicit
// root there is nowhere durable to put it, and a "cache" that is discarded
// with the run is only a slower way to fetch once.
func validateWorkspaceCache(ws *WorkspaceConfig) error {
	if ws.Cache == nil {
		return nil
	}

	if ws.Cache.MaxEntries < 0 {
		return fmt.Errorf("workspace.cache.max_entries %d must not be negative", ws.Cache.MaxEntries)
	}

	if ws.Cache.Resources && ws.Root == "" {
		return errors.New("workspace.cache.resources requires workspace.root — the cache must outlive the run that filled it, and a provider-owned temp root is removed at the end of it")
	}

	return nil
}
