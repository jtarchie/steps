package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0o750)
	if err == nil {
		err = os.WriteFile(path, []byte(content), 0o600)
	}

	if err != nil {
		t.Fatal(err)
	}
}

func TestTaskCacheShadowsTheInputItSitsIn(t *testing.T) {
	t.Parallel()

	bw := newBuild(t)
	dir := t.TempDir()
	key := TaskCacheKey("p", "j", "s", "src/node_modules")

	mustWrite(t, filepath.Join(dir, "src/node_modules/from-input"), "x")

	detach := bw.AttachCaches(t.Context(), dir, map[string]string{"src/node_modules": key})

	_, err := os.Stat(filepath.Join(dir, "src/node_modules/from-input"))
	if !os.IsNotExist(err) {
		t.Errorf("the input's copy survived under the cache: %v", err)
	}

	mustWrite(t, filepath.Join(dir, "src/node_modules/cached"), "y")
	detach()

	next := t.TempDir()
	bw.AttachCaches(t.Context(), next, map[string]string{"src/node_modules": key})

	_, err = os.Stat(filepath.Join(next, "src/node_modules/cached"))
	if err != nil {
		t.Errorf("the cache did not come back: %v", err)
	}
}

// TestTaskCacheConcurrentRunsLastFinisherWins: the second concurrent run starts empty, and what it files last is what the next run sees.
func TestTaskCacheConcurrentRunsLastFinisherWins(t *testing.T) {
	t.Parallel()

	bw := newBuild(t)
	key := TaskCacheKey("p", "j", "s", "c")
	caches := map[string]string{"c": key}

	seed := t.TempDir()
	detachSeed := bw.AttachCaches(t.Context(), seed, caches)
	mustWrite(t, filepath.Join(seed, "c/seed"), "0")
	detachSeed()

	first, second := t.TempDir(), t.TempDir()
	detachFirst := bw.AttachCaches(t.Context(), first, caches)
	detachSecond := bw.AttachCaches(t.Context(), second, caches)

	_, err := os.Stat(filepath.Join(second, "c/seed"))
	if !os.IsNotExist(err) {
		t.Fatalf("the second concurrent run shared the first's cache: %v", err)
	}

	mustWrite(t, filepath.Join(first, "c/who"), "first")
	mustWrite(t, filepath.Join(second, "c/who"), "second")
	detachFirst()
	detachSecond()

	next := t.TempDir()
	bw.AttachCaches(t.Context(), next, caches)

	got, err := os.ReadFile(filepath.Join(next, "c/who")) //nolint:gosec // a path under t.TempDir()
	if err != nil || string(got) != "second" {
		t.Errorf("kept %q (%v), want the last finisher's", got, err)
	}
}

func TestTaskCacheKeysSeparateEveryScope(t *testing.T) {
	t.Parallel()

	base := TaskCacheKey("p", "j", "s", "c")

	for name, other := range map[string]string{
		"pipeline": TaskCacheKey("q", "j", "s", "c"),
		"job":      TaskCacheKey("p", "k", "s", "c"),
		"step":     TaskCacheKey("p", "j", "t", "c"),
		"path":     TaskCacheKey("p", "j", "s", "d"),
		"boundary": TaskCacheKey("p", "js", "", "c"),
	} {
		if other == base {
			t.Errorf("a different %s shares the cache", name)
		}
	}

	if TaskCacheKey("p", "j", "s", "./c/") != base {
		t.Error("an uncleaned spelling of the same path names a different cache")
	}
}

// TestTaskCacheEmptyIsNotKept: an empty cache would spend an LRU slot on what the next attach makes anyway.
func TestTaskCacheEmptyIsNotKept(t *testing.T) {
	t.Parallel()

	bw := newBuild(t)
	build, _ := bw.(*isolatingBuild)
	key := TaskCacheKey("p", "j", "s", "c")

	dir := t.TempDir()
	bw.AttachCaches(t.Context(), dir, map[string]string{"c": key})()

	entry, _ := build.taskCaches.path(key)

	_, err := os.Stat(entry)
	if !os.IsNotExist(err) {
		t.Errorf("an untouched cache was filed: %v", err)
	}

	_, err = os.Stat(filepath.Join(dir, "c"))
	if !os.IsNotExist(err) {
		t.Errorf("the empty stand-in was left in the step directory: %v", err)
	}
}

// TestTaskCacheSurvivesARenameAcrossFilesystems: a cache inside an input sits in that input's btrfs snapshot, where rename answers EXDEV.
func TestTaskCacheSurvivesARenameAcrossFilesystems(t *testing.T) {
	rename = func(string, string) error { return &os.LinkError{Op: "rename", Err: errors.New("cross-device link")} }
	t.Cleanup(func() { rename = os.Rename })

	bw := newBuild(t)
	key := TaskCacheKey("p", "j", "s", "src/node_modules")
	caches := map[string]string{"src/node_modules": key}

	first := t.TempDir()
	detach := bw.AttachCaches(t.Context(), first, caches)
	mustWrite(t, filepath.Join(first, "src/node_modules/pkg/index.js"), "x")
	detach()

	_, err := os.Stat(filepath.Join(first, "src/node_modules"))
	if !os.IsNotExist(err) {
		t.Errorf("the cache stayed in the step directory after the copy: %v", err)
	}

	second := t.TempDir()
	bw.AttachCaches(t.Context(), second, caches)

	got, err := os.ReadFile(filepath.Join(second, "src/node_modules/pkg/index.js")) //nolint:gosec // a path under t.TempDir()
	if err != nil || string(got) != "x" {
		t.Errorf("the cache did not come back by copy: %q, %v", got, err)
	}
}
