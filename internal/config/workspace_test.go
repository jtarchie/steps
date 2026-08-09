package config

import (
	"strings"
	"testing"
)

func TestValidateWorkspaceCacheRequiresRoot(t *testing.T) {
	t.Parallel()

	cfg := &Config{Workspace: &WorkspaceConfig{
		Strategy: "copy",
		Cache:    &CacheConfig{Resources: true},
	}}

	err := cfg.validateWorkspace()
	if err == nil {
		t.Fatal("expected cache.resources without root: to be rejected")
	}

	if !strings.Contains(err.Error(), "requires workspace.root") {
		t.Errorf("error = %v, want it to name the missing root", err)
	}
}

// TestValidateWorkspaceCacheDisabledNeedsNoRoot pins that only ENABLING the
// cache carries the root requirement: `cache: {resources: false}` is how a
// pipeline turns it off while keeping its tuning, and that must stay legal.
func TestValidateWorkspaceCacheDisabledNeedsNoRoot(t *testing.T) {
	t.Parallel()

	cfg := &Config{Workspace: &WorkspaceConfig{
		Strategy: "copy",
		Cache:    &CacheConfig{Resources: false, MaxEntries: 10},
	}}

	err := cfg.validateWorkspace()
	if err != nil {
		t.Errorf("validateWorkspace: %v", err)
	}
}

func TestValidateWorkspaceCacheRejectsNegativeMaxEntries(t *testing.T) {
	t.Parallel()

	cfg := &Config{Workspace: &WorkspaceConfig{
		Strategy: "copy",
		Root:     "/tmp/ws",
		Cache:    &CacheConfig{Resources: true, MaxEntries: -1},
	}}

	err := cfg.validateWorkspace()
	if err == nil {
		t.Error("expected a negative max_entries to be rejected")
	}
}

func TestCacheHelpersDefaultSafely(t *testing.T) {
	t.Parallel()

	var nilWS *WorkspaceConfig

	if nilWS.CacheEnabled() {
		t.Error("a nil workspace should not report the cache as enabled")
	}

	if got := nilWS.CacheMaxEntries(); got != DefaultCacheMaxEntries {
		t.Errorf("CacheMaxEntries() = %d, want the default %d", got, DefaultCacheMaxEntries)
	}

	ws := &WorkspaceConfig{Cache: &CacheConfig{Resources: true}}
	if got := ws.CacheMaxEntries(); got != DefaultCacheMaxEntries {
		t.Errorf("CacheMaxEntries() = %d, want the default when unset", got)
	}
}
