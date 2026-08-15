package resource

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// countingConfig returns a Config with one resource ("thing") whose check:
// command appends a line to counterPath every time it runs, and the number
// of lines currently in counterPath (0 initially).
func countingConfig(t *testing.T, counterPath string) *config.Config {
	t.Helper()

	return &config.Config{
		ResourceTypes: []config.ResourceType{{
			Name: "dummy",
			Config: config.ResourceTypeConfig{
				Check: "echo ran >> " + counterPath + `; echo '[{"ref":"v1"}]'`,
			},
		}},
		Resources: []config.Resource{{
			Name: "thing",
			Type: "dummy",
		}},
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // path is a t.TempDir()-scoped counter file this test wrote itself
	if os.IsNotExist(err) {
		return 0
	}

	if err != nil {
		t.Fatal(err)
	}

	count := 0
	for _, b := range data {
		if b == '\n' {
			count++
		}
	}

	return count
}

func TestCacheResolveVersionsCachedReusesResult(t *testing.T) {
	t.Parallel()

	counter := filepath.Join(t.TempDir(), "counter.txt")
	cfg := countingConfig(t, counter)
	cache := NewCache()

	step := config.Step{Get: "thing"}

	_, _, _, err := cache.ResolveVersionsCached(context.Background(), cfg, step, nil)
	if err != nil {
		t.Fatalf("ResolveVersionsCached (first call): %v", err)
	}

	if got := countLines(t, counter); got != 1 {
		t.Fatalf("check ran %d times after first call, want 1", got)
	}

	_, _, _, err = cache.ResolveVersionsCached(context.Background(), cfg, step, nil)
	if err != nil {
		t.Fatalf("ResolveVersionsCached (second call): %v", err)
	}

	if got := countLines(t, counter); got != 1 {
		t.Errorf("check ran %d times after second call through the same cache, want still 1 (cached)", got)
	}
}

func TestCacheNilAlwaysMisses(t *testing.T) {
	t.Parallel()

	counter := filepath.Join(t.TempDir(), "counter.txt")
	cfg := countingConfig(t, counter)

	var cache *Cache // nil receiver

	step := config.Step{Get: "thing"}

	_, _, _, err := cache.ResolveVersionsCached(context.Background(), cfg, step, nil)
	if err != nil {
		t.Fatalf("ResolveVersionsCached (first call): %v", err)
	}

	_, _, _, err = cache.ResolveVersionsCached(context.Background(), cfg, step, nil)
	if err != nil {
		t.Fatalf("ResolveVersionsCached (second call): %v", err)
	}

	if got := countLines(t, counter); got != 2 {
		t.Errorf("check ran %d times through a nil cache, want 2 (nil always misses)", got)
	}
}

// TestCacheWithLastCheckedFeedsTheCheck covers the seam that keeps this
// package free of the store: the caller holds the cursor, the Cache carries
// the lookup, and the check sees the answer. It is looked up by the RESOLVED
// resource name — the get step here aliases it (get: alias, resource: thing),
// which is exactly the case a name taken from step.Get would get wrong.
func TestCacheWithLastCheckedFeedsTheCheck(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{{
			Name: "dummy",
			Config: config.ResourceTypeConfig{
				Check: `printf '[{"ref": "%s"}]' '{{ index .version "ref" | default "cold" }}'`,
			},
		}},
		Resources: []config.Resource{{Name: "thing", Type: "dummy"}},
	}

	var asked []string

	cache := NewCache(WithLastChecked(func(resourceName string) map[string]any {
		asked = append(asked, resourceName)

		return map[string]any{"ref": "seeded"}
	}))

	step := config.Step{Get: "alias", Resource: "thing"}

	_, _, versions, err := cache.ResolveVersionsCached(context.Background(), cfg, step, nil)
	if err != nil {
		t.Fatalf("ResolveVersionsCached: %v", err)
	}

	if len(versions) != 1 || versions[0]["ref"] != "seeded" {
		t.Errorf("versions = %+v, want the check to have seen the cursor", versions)
	}

	if len(asked) != 1 || asked[0] != "thing" {
		t.Errorf("cursor looked up for %v, want the resolved resource name [thing]", asked)
	}

	// A cache with no cursor lookup is every caller that has no store to ask,
	// and must still check — as an empty map, not a missing key.
	plain := NewCache()

	_, _, versions, err = plain.ResolveVersionsCached(context.Background(), cfg, step, nil)
	if err != nil {
		t.Fatalf("ResolveVersionsCached (no cursor): %v", err)
	}

	if len(versions) != 1 || versions[0]["ref"] != "cold" {
		t.Errorf("versions = %+v, want the default with no cursor supplied", versions)
	}
}

func TestCacheKeyedByResourceName(t *testing.T) {
	t.Parallel()

	counter := filepath.Join(t.TempDir(), "counter.txt")
	cfg := countingConfig(t, counter)
	cfg.Resources = append(cfg.Resources, config.Resource{Name: "other", Type: "dummy"})

	cache := NewCache()

	_, _, _, err := cache.ResolveVersionsCached(context.Background(), cfg, config.Step{Get: "thing"}, nil)
	if err != nil {
		t.Fatalf("ResolveVersionsCached(thing): %v", err)
	}

	_, _, _, err = cache.ResolveVersionsCached(context.Background(), cfg, config.Step{Get: "other"}, nil)
	if err != nil {
		t.Fatalf("ResolveVersionsCached(other): %v", err)
	}

	if got := countLines(t, counter); got != 2 {
		t.Errorf("check ran %d times for two distinct resources, want 2 (each resource name is its own cache entry)", got)
	}
}
