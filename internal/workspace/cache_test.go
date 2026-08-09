package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// cachingBuild builds a provider with the resource cache enabled over root,
// returning one build workspace from it.
func cachingBuild(t *testing.T, root string, maxEntries int) CachingBuild {
	t.Helper()

	provider, err := NewProvider(&config.WorkspaceConfig{
		Strategy: "copy",
		Root:     root,
		Cache:    &config.CacheConfig{Resources: true, MaxEntries: maxEntries},
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	bw, err := provider.NewBuild(context.Background(), "build")
	if err != nil {
		t.Fatal(err)
	}

	caching, ok := bw.(CachingBuild)
	if !ok {
		t.Fatal("an isolating build should implement CachingBuild")
	}

	return caching
}

// TestResourceCacheReusesAcrossBuilds is the whole feature: the second build
// gets the first build's content without the fetch running again. Agent and
// put steps make their chains unskippable, so without this every real run
// re-fetches every get.
func TestResourceCacheReusesAcrossBuilds(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	fetches := 0

	fetch := func(dir string) error {
		fetches++

		return os.WriteFile(filepath.Join(dir, "NOTES.txt"), []byte("fetched once"), 0o600)
	}

	first := cachingBuild(t, root, 10)

	dir, err := first.FetchResource(context.Background(), "repo", "key-abc", fetch)
	if err != nil {
		t.Fatalf("FetchResource: %v", err)
	}

	if got := readFile(t, filepath.Join(dir, "NOTES.txt")); got != "fetched once" {
		t.Errorf("first build content = %q", got)
	}

	second := cachingBuild(t, root, 10)

	dir, err = second.FetchResource(context.Background(), "repo", "key-abc", fetch)
	if err != nil {
		t.Fatalf("FetchResource (second build): %v", err)
	}

	if fetches != 1 {
		t.Errorf("fetches = %d, want 1 — the second build should have reused the cached version", fetches)
	}

	if got := readFile(t, filepath.Join(dir, "NOTES.txt")); got != "fetched once" {
		t.Errorf("second build content = %q, want the cached content", got)
	}
}

// TestResourceCacheDistinguishesKeys guards the obvious way to get this
// catastrophically wrong: serving one version's bytes for another.
func TestResourceCacheDistinguishesKeys(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	write := func(content string) func(string) error {
		return func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "V"), []byte(content), 0o600)
		}
	}

	bw := cachingBuild(t, root, 10)

	_, err := bw.FetchResource(context.Background(), "repo", "key-v1", write("v1"))
	if err != nil {
		t.Fatal(err)
	}

	dir, err := bw.FetchResource(context.Background(), "other", "key-v2", write("v2"))
	if err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, filepath.Join(dir, "V")); got != "v2" {
		t.Errorf("content = %q, want v2 — a different key must not hit v1's entry", got)
	}
}

// TestResourceCacheDoesNotStoreAFailedFetch pins the ordering: caching before
// knowing the fetch succeeded would poison every later build with a
// half-written tree, which is far worse than fetching again.
func TestResourceCacheDoesNotStoreAFailedFetch(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	boom := errors.New("fetch failed")

	bw := cachingBuild(t, root, 10)

	_, err := bw.FetchResource(context.Background(), "repo", "key-abc", func(dir string) error {
		_ = os.WriteFile(filepath.Join(dir, "PARTIAL"), []byte("half"), 0o600)

		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the fetch's own error", err)
	}

	called := false

	_, err = bw.FetchResource(context.Background(), "repo", "key-abc", func(dir string) error {
		called = true

		return os.WriteFile(filepath.Join(dir, "GOOD"), []byte("whole"), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}

	if !called {
		t.Error("the retry reused a cache entry seeded by a FAILED fetch")
	}
}

// TestResourceCacheEmptyKeyIsNotCached covers the caller's escape hatch: the
// pipeline passes "" when it could not compute a key, and that must mean
// "fetch normally", never "cache everything under one name".
func TestResourceCacheEmptyKeyIsNotCached(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	fetches := 0

	fetch := func(string) error {
		fetches++

		return nil
	}

	bw := cachingBuild(t, root, 10)

	for range 2 {
		_, err := bw.FetchResource(context.Background(), "repo", "", fetch)
		if err != nil {
			t.Fatal(err)
		}
	}

	if fetches != 2 {
		t.Errorf("fetches = %d, want 2 — an empty key must not be cached", fetches)
	}
}

// TestResourceCacheRejectsATraversingKey guards the one input that could let a
// cache entry escape its directory.
func TestResourceCacheRejectsATraversingKey(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cache, err := newResourceCache(copyBackend{}, root, 10)
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"", "..", "../escape", "a/b", `a\b`} {
		if _, ok := cache.entryPath(key); ok {
			t.Errorf("entryPath(%q) was accepted, want it refused", key)
		}
	}

	if _, ok := cache.entryPath("abc123"); !ok {
		t.Error("entryPath rejected an ordinary hex key")
	}
}

// TestResourceCacheEvictsLeastRecentlyUsed keeps an unbounded cache from
// filling the disk on a long-lived watch host.
func TestResourceCacheEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	bw := cachingBuild(t, root, 2)

	write := func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "V"), []byte("x"), 0o600)
	}

	for _, key := range []string{"key-1", "key-2", "key-3"} {
		_, err := bw.FetchResource(context.Background(), "repo", key, write)
		if err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(root, cacheDirName))
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) > 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}

		t.Errorf("cache holds %v, want at most 2 entries", names)
	}
}

// TestSweepStaleBuildsSpareTheCache is the interaction that would otherwise
// quietly undo the whole feature: the crash sweep and the cache share a root,
// and a sweep that took everything would throw away the asset with the
// garbage.
func TestSweepStaleBuildsSpareTheCache(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	bw := cachingBuild(t, root, 10)

	_, err := bw.FetchResource(context.Background(), "repo", "key-abc", func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "NOTES.txt"), []byte("cached"), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}

	entry := filepath.Join(root, cacheDirName, "key-abc")

	_, err = os.Stat(entry)
	if err != nil {
		t.Fatalf("expected a cache entry at %s: %v", entry, err)
	}

	// A fresh provider over the same root sweeps on Validate.
	provider, err := NewProvider(&config.WorkspaceConfig{
		Strategy: "copy",
		Root:     root,
		Cache:    &config.CacheConfig{Resources: true},
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = provider.Close() }()

	err = provider.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	_, err = os.Stat(entry)
	if err != nil {
		t.Errorf("the startup sweep removed a cache entry: %v", err)
	}
}

// TestNoCacheWithoutOptIn keeps the default behavior exactly as it was: the
// in: of every get runs on every build unless a pipeline says otherwise.
func TestNoCacheWithoutOptIn(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	fetches := 0

	fetch := func(string) error {
		fetches++

		return nil
	}

	for range 2 {
		provider, err := NewProvider(&config.WorkspaceConfig{Strategy: "copy", Root: root}, false)
		if err != nil {
			t.Fatal(err)
		}

		bw, err := provider.NewBuild(context.Background(), "build")
		if err != nil {
			t.Fatal(err)
		}

		caching, ok := bw.(CachingBuild)
		if !ok {
			t.Fatal("an isolating build should implement CachingBuild even with the cache off")
		}

		_, err = caching.FetchResource(context.Background(), "repo", "key-abc", fetch)
		if err != nil {
			t.Fatal(err)
		}

		_ = provider.Close()
	}

	if fetches != 2 {
		t.Errorf("fetches = %d, want 2 — the cache must be off unless the pipeline opts in", fetches)
	}
}

// TestResourceCacheRefusesASymlinkedEntry is the guard materializeSpace and
// Capture already apply, extended to the cache: the copy backend's
// `cp -R -P -p src/. dst` dereferences a symlink at src itself despite -P, and
// a step's shell commands can reach the cache by absolute path.
func TestResourceCacheRefusesASymlinkedEntry(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	bw := cachingBuild(t, root, 10)

	_, err := bw.FetchResource(context.Background(), "repo", "key-abc", func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "V"), []byte("real"), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}

	// Swap the cache entry for a symlink to somewhere it must never copy from.
	secret := t.TempDir()

	err = os.WriteFile(filepath.Join(secret, "SECRET"), []byte("do not exfiltrate"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	entry := filepath.Join(root, cacheDirName, "key-abc")

	err = os.RemoveAll(entry)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink(secret, entry)
	if err != nil {
		t.Fatal(err)
	}

	fetched := false

	dir, err := bw.FetchResource(context.Background(), "repo2", "key-abc", func(d string) error {
		fetched = true

		return os.WriteFile(filepath.Join(d, "V"), []byte("refetched"), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}

	if !fetched {
		t.Error("a symlinked cache entry was restored instead of refused")
	}

	_, statErr := os.Stat(filepath.Join(dir, "SECRET"))
	if statErr == nil {
		t.Error("the symlink target's contents were copied into the resource directory")
	}
}

// TestResourceCacheRefusesToStoreASymlink is the mirror: an in: that replaced
// its own output directory with a symlink must not get that target copied into
// the cache and served to every later build.
func TestResourceCacheRefusesToStoreASymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	bw := cachingBuild(t, root, 10)

	secret := t.TempDir()

	err := os.WriteFile(filepath.Join(secret, "SECRET"), []byte("do not exfiltrate"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = bw.FetchResource(context.Background(), "repo", "key-abc", func(dir string) error {
		// Stand in for an in: that swapped its directory for a symlink.
		rmErr := os.RemoveAll(dir)
		if rmErr != nil {
			return fmt.Errorf("removing %q: %w", dir, rmErr)
		}

		linkErr := os.Symlink(secret, dir)
		if linkErr != nil {
			return fmt.Errorf("symlinking %q: %w", dir, linkErr)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	entry := filepath.Join(root, cacheDirName, "key-abc")

	_, statErr := os.Stat(entry)
	if statErr == nil {
		t.Error("a symlinked fetch result was stored in the cache")
	}
}
