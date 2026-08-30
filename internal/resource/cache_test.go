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

// TestCacheWithResolvedVersionsSkipsTheCheck covers the seam that lets a
// caller hand over versions it already resolved.
//
// The check must not run at all: this exists because `steps web` has
// already asked, and asking again does not merely cost a round trip — a
// cursor-driven check answers a DIFFERENT question the second time, which is
// how a triggered job came to process its whole window instead of the items
// it was triggered for.
//
// The lookup is by RESOLVED resource name, which the aliased get here proves:
// a name taken from step.Get would miss.
func TestCacheWithResolvedVersionsSkipsTheCheck(t *testing.T) {
	t.Parallel()

	counter := filepath.Join(t.TempDir(), "counter.txt")
	cfg := countingConfig(t, counter)

	var asked []string

	cache := NewCache(WithResolvedVersions(func(resourceName string) []map[string]any {
		asked = append(asked, resourceName)

		return []map[string]any{{"ref": "supplied"}}
	}))

	step := config.Step{Get: "alias", Resource: "thing"}

	_, _, versions, err := cache.ResolveVersionsCached(context.Background(), cfg, step, nil)
	if err != nil {
		t.Fatalf("ResolveVersionsCached: %v", err)
	}

	if len(versions) != 1 || versions[0]["ref"] != "supplied" {
		t.Errorf("versions = %+v, want the supplied version", versions)
	}

	if got := countLines(t, counter); got != 0 {
		t.Errorf("check ran %d times, want 0 — the caller already resolved this", got)
	}

	if len(asked) != 1 || asked[0] != "thing" {
		t.Errorf("looked up %v, want the resolved resource name [thing]", asked)
	}
}

// TestCacheWithResolvedVersionsFallsBackToChecking: a resource nobody
// supplied still checks. That is every get beside a triggered one, and every
// resource under a manual run — the same code path, not a second one.
func TestCacheWithResolvedVersionsFallsBackToChecking(t *testing.T) {
	t.Parallel()

	counter := filepath.Join(t.TempDir(), "counter.txt")
	cfg := countingConfig(t, counter)

	cache := NewCache(WithResolvedVersions(func(string) []map[string]any { return nil }))

	_, _, versions, err := cache.ResolveVersionsCached(context.Background(), cfg, config.Step{Get: "thing"}, nil)
	if err != nil {
		t.Fatalf("ResolveVersionsCached: %v", err)
	}

	if len(versions) != 1 || versions[0]["ref"] != "v1" {
		t.Errorf("versions = %+v, want the check's own answer", versions)
	}

	if got := countLines(t, counter); got != 1 {
		t.Errorf("check ran %d times, want 1", got)
	}
}

// TestCacheSuppliedEmptyIsNotAbsent: a caller that resolved NO versions is
// not the same as a caller that supplied nothing. Re-deriving here would
// hand the job the whole window precisely when the poll said there was
// nothing new.
func TestCacheSuppliedEmptyIsNotAbsent(t *testing.T) {
	t.Parallel()

	counter := filepath.Join(t.TempDir(), "counter.txt")
	cfg := countingConfig(t, counter)

	cache := NewCache(WithResolvedVersions(func(string) []map[string]any { return []map[string]any{} }))

	step := config.Step{Get: "thing", Version: "every"}

	_, _, versions, err := cache.ResolveVersionsCached(context.Background(), cfg, step, nil)
	if err != nil {
		t.Fatalf("ResolveVersionsCached: %v", err)
	}

	if len(versions) != 0 {
		t.Errorf("versions = %+v, want none", versions)
	}

	if got := countLines(t, counter); got != 0 {
		t.Errorf("check ran %d times, want 0 — an empty supply is an answer", got)
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

// TestPinnedGetIgnoresSuppliedVersions: a pin is an instruction, not a
// question. `steps watch --pin ref=abc123` has to find abc123 even after the
// resource has moved on, and matching it against the handful of versions the
// last poll observed would fail with "no version matches pin" — for a version
// that plainly exists. The consumed filter exempts pinned runs for the same
// reason.
func TestPinnedGetIgnoresSuppliedVersions(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{{
			Name:   "dummy",
			Config: config.ResourceTypeConfig{Check: `printf '[{"ref":"old"},{"ref":"new"}]'`},
		}},
		Resources: []config.Resource{{Name: "thing", Type: "dummy"}},
	}

	// A poll that only ever saw the newest version.
	newCache := func() *Cache {
		return NewCache(WithResolvedVersions(func(string) []map[string]any {
			return []map[string]any{{"ref": "new"}}
		}))
	}

	// A CLI pin reaches back past what the poll observed.
	_, _, versions, err := newCache().ResolveVersionsCached(context.Background(), cfg,
		config.Step{Get: "thing"}, map[string]string{"ref": "old"})
	if err != nil {
		t.Fatalf("--pin against supplied versions: %v", err)
	}

	if len(versions) != 1 || versions[0]["ref"] != "old" {
		t.Errorf("versions = %+v, want the pinned one", versions)
	}

	// And a step-level version: pin, which is the same instruction written in
	// the pipeline instead of on the command line.
	_, _, versions, err = newCache().ResolveVersionsCached(context.Background(), cfg,
		config.Step{Get: "thing", Version: map[string]any{"ref": "old"}}, nil)
	if err != nil {
		t.Fatalf("step version: pin against supplied versions: %v", err)
	}

	if len(versions) != 1 || versions[0]["ref"] != "old" {
		t.Errorf("versions = %+v, want the pinned one", versions)
	}

	// Unpinned still takes what the poll supplied, without checking.
	_, _, versions, err = newCache().ResolveVersionsCached(context.Background(), cfg,
		config.Step{Get: "thing"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(versions) != 1 || versions[0]["ref"] != "new" {
		t.Errorf("versions = %+v, want the supplied version", versions)
	}
}
