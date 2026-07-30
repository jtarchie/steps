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

	return nil
}
