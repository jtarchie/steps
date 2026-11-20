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

func TestCacheSuppressedCountsWhatTheConsumedFilterDropped(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{{Name: "dummy"}},
		Resources:     []config.Resource{{Name: "thing", Type: "dummy"}},
	}

	cache := NewCache(
		WithResolvedVersions(func(string) []map[string]any {
			return []map[string]any{{"ref": "a"}, {"ref": "b"}, {"ref": "c"}}
		}),
		WithConsumed(func(_ string, version map[string]any) bool { return version["ref"] == "a" }),
	)

	step := config.Step{Get: "thing", Version: "every"}

	_, _, versions, err := cache.ResolveVersionsCached(context.Background(), cfg, step, nil)
	if err != nil || len(versions) != 2 || cache.Suppressed(step) != 1 {
		t.Errorf("versions = %v, %v, suppressed %d; want b and c, with a counted as taken", versions, err, cache.Suppressed(step))
	}
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

// TestPinnedGetChecksWhenSuppliedVersionsLackIt: a pin is an instruction, not a
// question. `steps watch --pin ref=abc123` has to find abc123 even after the
// resource has moved on, and matching it against the handful of versions the
// last poll observed would fail with "no version matches pin" — for a version
// that plainly exists. The consumed filter exempts pinned runs for the same
// reason.
func TestPinnedGetChecksWhenSuppliedVersionsLackIt(t *testing.T) {
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

// TestPinnedGetSelectsASuppliedVersionTheCheckCannotReport: a webhook's check
// reports nothing, so a delivery exists only as recorded — a pin has to be
// answered from there, without asking the check (#274).
func TestPinnedGetSelectsASuppliedVersionTheCheckCannotReport(t *testing.T) {
	t.Parallel()

	counter := filepath.Join(t.TempDir(), "counter.txt")
	cfg := countingConfig(t, counter)
	cfg.ResourceTypes[0].Config.Check = "echo ran >> " + counter + "; echo '[]'"

	cache := NewCache(WithResolvedVersions(func(string) []map[string]any {
		return []map[string]any{{"id": "first"}, {"id": "second"}}
	}))

	_, _, versions, err := cache.ResolveVersionsCached(context.Background(), cfg,
		config.Step{Get: "thing"}, map[string]string{"id": "first"})
	if err != nil {
		t.Fatalf("--pin against a recorded version: %v", err)
	}

	if len(versions) != 1 || versions[0]["id"] != "first" {
		t.Errorf("versions = %+v, want the pinned recorded one", versions)
	}

	_, _, versions, err = cache.ResolveVersionsCached(context.Background(), cfg,
		config.Step{Get: "thing", Version: map[string]any{"id": "second"}}, nil)
	if err != nil {
		t.Fatalf("step version: pin against a recorded version: %v", err)
	}

	if len(versions) != 1 || versions[0]["id"] != "second" {
		t.Errorf("versions = %+v, want the step's pinned recorded one", versions)
	}

	if got := countLines(t, counter); got != 0 {
		t.Errorf("check ran %d times, want 0 — the pins were already recorded", got)
	}
}

// A CLI pin names one resource's version, and a job gets several: prs review
// pinned to number=23407 failed its image get, whose versions are {digest},
// with "no version matches pin" before a step ran. A get of another shape
// takes its own pin or the latest, and is not sent to a check for it.
func TestCLIPinPassesOverAGetWhoseVersionsLackItsFields(t *testing.T) {
	t.Parallel()

	counter := filepath.Join(t.TempDir(), "counter.txt")
	cfg := countingConfig(t, counter)
	cfg.ResourceTypes[0].Config.Check = "echo ran >> " + counter + "; echo '[]'"
	cfg.Resources = append(cfg.Resources, config.Resource{Name: "image", Type: cfg.Resources[0].Type})

	cache := NewCache(WithResolvedVersions(func(name string) []map[string]any {
		if name == "image" {
			return []map[string]any{{"digest": "sha256:old"}, {"digest": "sha256:new"}}
		}

		return []map[string]any{{"number": "86", "sha": "a"}, {"number": "87", "sha": "b"}, {"number": "88", "sha": "c"}}
	}))
	pin := map[string]string{"number": "87"}

	cases := []struct {
		name         string
		step         config.Step
		field, value string
	}{
		{"the pinned get, though every", config.Step{Get: "thing", Version: "every"}, "number", "87"},
		{"a get the pin is not about takes the latest", config.Step{Get: "image"}, "digest", "sha256:new"},
		{"its own version: pin still holds", config.Step{Get: "base", Resource: "image", Version: map[string]any{"digest": "sha256:old"}}, "digest", "sha256:old"},
		{"an every get still builds one set", config.Step{Get: "each", Resource: "image", Version: "every"}, "digest", "sha256:new"},
	}

	for _, tc := range cases {
		_, _, versions, err := cache.ResolveVersionsCached(context.Background(), cfg, tc.step, pin)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)

			continue
		}

		if len(versions) != 1 || versions[0][tc.field] != tc.value {
			t.Errorf("%s: versions = %+v, want %s=%s alone", tc.name, versions, tc.field, tc.value)
		}
	}

	if got := countLines(t, counter); got != 0 {
		t.Errorf("check ran %d times, want 0: every answer was recorded", got)
	}
}

// A field the versions carry but never with that value is still this
// resource's pin: it is owed a check, and then an error.
func TestCLIPinWhoseFieldIsCarriedButNeverEqualStillFails(t *testing.T) {
	t.Parallel()

	counter := filepath.Join(t.TempDir(), "counter.txt")
	cfg := countingConfig(t, counter)
	cfg.ResourceTypes[0].Config.Check = "echo ran >> " + counter + "; echo '[]'"

	cache := NewCache(WithResolvedVersions(func(string) []map[string]any {
		return []map[string]any{{"number": "86"}}
	}))

	_, _, _, err := cache.ResolveVersionsCached(context.Background(), cfg, config.Step{Get: "thing"}, map[string]string{"number": "999"})
	if err == nil {
		t.Error("the pin resolved; want an error")
	}

	if got := countLines(t, counter); got != 1 {
		t.Errorf("check ran %d times, want 1", got)
	}
}
