package config

// The workspace: block: per-step filesystem isolation strategy and its
// strategy-specific options.

import (
	"errors"
	"fmt"
)

// WorkspaceConfig opts a pipeline into Concourse-style per-step workspace
// isolation: when set, task/agent/put steps materialize a directory built
// from their own declared inputs:/outputs: (see Step, Task) instead of
// sharing the build's directory with every other step. This is corruption
// hygiene, not a security sandbox — a step's shell commands can still reach
// outside the materialized directory via absolute paths, exactly as today.
type WorkspaceConfig struct {
	// Strategy is "copy" (portable; uses copy-on-write when the underlying
	// filesystem supports it — APFS clonefile on macOS, reflink on Linux —
	// and falls back to a plain recursive copy otherwise) or "btrfs" (Linux
	// only; instant copy-on-write via btrfs subvolume snapshots).
	Strategy string `yaml:"strategy"`
	// Root is where isolated build workspaces are materialized. Optional for
	// strategy: copy (defaults to the system temp directory); required for
	// strategy: btrfs, since the system temp directory (often tmpfs) is
	// commonly not itself a btrfs filesystem.
	Root string `yaml:"root,omitempty"`
	// Options holds strategy-specific tuning; currently btrfs only.
	Options WorkspaceOptions `yaml:"options,omitempty"`
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
	// grows until the disk does not, which on a long-lived watch host is a
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

// WorkspaceOptions holds strategy-specific workspace tuning.
type WorkspaceOptions struct {
	// Compression sets a btrfs subvolume's compression property: "zstd",
	// "lzo", "zlib", or "none". Valid only for strategy: btrfs.
	Compression string `yaml:"compression,omitempty"`
}

var (
	workspaceStrategies = map[string]bool{"copy": true, "btrfs": true}
	compressionValues   = map[string]bool{"": true, "zstd": true, "lzo": true, "zlib": true, "none": true}
)

func (c *Config) validateWorkspace() error {
	ws := c.Workspace
	if ws == nil {
		return nil
	}

	if !workspaceStrategies[ws.Strategy] {
		return fmt.Errorf("workspace.strategy %q must be one of copy, btrfs", ws.Strategy)
	}

	if ws.Strategy == "btrfs" && ws.Root == "" {
		return errors.New("workspace.root is required for strategy: btrfs (the system temp directory is commonly not a btrfs filesystem)")
	}

	if !compressionValues[ws.Options.Compression] {
		return fmt.Errorf("workspace.options.compression %q must be one of zstd, lzo, zlib, none", ws.Options.Compression)
	}

	if ws.Options.Compression != "" && ws.Strategy != "btrfs" {
		return fmt.Errorf("workspace.options.compression is only valid for strategy: btrfs, not %q", ws.Strategy)
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
