package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// interruptedBackend is the copy backend whose materialize, while failing is
// set, does what an interrupted cp does: creates dst and then gives up, so
// the destination exists and holds none of the bytes. With refuses, it fails
// before creating anything.
type interruptedBackend struct {
	copyBackend

	failing *atomic.Bool
	refuses bool
}

func (b interruptedBackend) materialize(ctx context.Context, src, dst string) error {
	if !b.failing.Load() {
		return b.copyBackend.materialize(ctx, src, dst)
	}

	if b.refuses {
		return errors.New("materialize refused")
	}

	err := os.MkdirAll(dst, 0o750)
	if err != nil {
		return fmt.Errorf("creating %q: %w", dst, err)
	}

	return errors.New("materialize interrupted")
}

// interruptible swaps an interruptedBackend into every place provider hands
// its backend to, returning the switch that makes materialize fail.
func interruptible(t *testing.T, provider Provider) *atomic.Bool {
	t.Helper()

	p, ok := provider.(*isolatingProvider)
	if !ok {
		t.Fatal("not an isolating provider")
	}

	failing := &atomic.Bool{}
	backend := interruptedBackend{failing: failing}

	p.backend = backend

	if p.cache != nil {
		p.cache.backend = backend
	}

	if p.stepCache != nil {
		p.stepCache.backend = backend
	}

	return failing
}

func assertNoStagingLeft(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if strings.Contains(entry.Name(), stagedMarker) {
			t.Errorf("staging directory %q left behind in %s", entry.Name(), dir)
		}
	}
}

func TestStepCacheInterruptedStoreIsAMiss(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	provider := stepCacheProvider(t, root)
	failing := interruptible(t, provider)
	req := stepRequest()

	first := seededBuild(t, provider, "notes")
	res := mustRestore(t, first, req)
	produceOutput(t, first, req, "the summary")

	failing.Store(true)

	err := first.StoreStep(context.Background(), res.Key, req)
	if err == nil {
		t.Fatal("a store whose materialize failed reported success")
	}

	failing.Store(false)
	assertNoStagingLeft(t, filepath.Join(root, stepCacheDirName))

	second := seededBuild(t, provider, "notes")

	if mustRestore(t, second, req).Hit {
		t.Fatal("an interrupted store was served as a hit")
	}
}

// resourceFetcher is a resource-cached provider over root and a FetchResource
// of one fixed version from a fresh build, counting the real fetches.
func resourceFetcher(t *testing.T, root string) (Provider, func() string, *int) {
	t.Helper()

	provider, err := NewProvider(&config.WorkspaceConfig{
		Strategy: "copy",
		Root:     root,
		Cache:    &config.CacheConfig{Resources: true, MaxEntries: 10},
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	fetches := 0

	fetch := func(dir string) error {
		fetches++

		return os.WriteFile(filepath.Join(dir, "NOTES.txt"), []byte("fetched"), 0o600)
	}

	fetchOnce := func() string {
		bw, buildErr := provider.NewBuild(context.Background(), "build")
		if buildErr != nil {
			t.Fatal(buildErr)
		}

		t.Cleanup(func() { _ = bw.Close() })

		dir, fetchErr := bw.FetchResource(context.Background(), "repo", "key-abc", fetch)
		if fetchErr != nil {
			t.Fatalf("FetchResource: %v", fetchErr)
		}

		return dir
	}

	return provider, fetchOnce, &fetches
}

func TestResourceCacheInterruptedStoreIsAMiss(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	provider, fetchOnce, fetches := resourceFetcher(t, root)
	failing := interruptible(t, provider)

	failing.Store(true)
	fetchOnce()
	failing.Store(false)

	assertNoStagingLeft(t, filepath.Join(root, cacheDirName))

	dir := fetchOnce()

	if *fetches != 2 {
		t.Errorf("fetches = %d, want 2: an interrupted store must not be served as a hit", *fetches)
	}

	if got := readFile(t, filepath.Join(dir, "NOTES.txt")); got != "fetched" {
		t.Errorf("content = %q, want the fetched content", got)
	}
}

// A restore that fails before creating anything must still leave the fetch
// the directory it was handed.
func TestResourceCacheRefusedRestoreStillFetches(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	provider, fetchOnce, fetches := resourceFetcher(t, root)
	failing := interruptible(t, provider)

	fetchOnce()

	provider.(*isolatingProvider).cache.backend = interruptedBackend{failing: failing, refuses: true}
	failing.Store(true)

	dir := fetchOnce()

	if *fetches != 2 {
		t.Errorf("fetches = %d, want 2: a refused restore falls back to fetching", *fetches)
	}

	if got := readFile(t, filepath.Join(dir, "NOTES.txt")); got != "fetched" {
		t.Errorf("content = %q, want the fetched content", got)
	}
}

func TestCaptureInterruptedLeavesThePreviousArtifact(t *testing.T) {
	t.Parallel()

	provider := stepCacheProvider(t, t.TempDir())
	failing := interruptible(t, provider)
	req := stepRequest()

	bw := seededBuild(t, provider, "notes")
	produceOutput(t, bw, req, "the first capture")

	space, err := bw.TaskSpace(context.Background(), "again", req.Inputs, req.Outputs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = space.Close() }()

	err = os.WriteFile(filepath.Join(space.Dir(), "report", "summary.md"), []byte("the second capture"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	failing.Store(true)

	err = space.Capture(context.Background())
	if err == nil {
		t.Fatal("a capture whose materialize failed reported success")
	}

	failing.Store(false)

	artifacts := bw.(*isolatingBuild).artifacts
	assertNoStagingLeft(t, artifacts)

	if got := readFile(t, filepath.Join(artifacts, "report", "summary.md")); got != "the first capture" {
		t.Errorf("artifact = %q, want the previous capture intact", got)
	}
}

// A run killed mid-capture leaves its staging directory in the artifact store
// a resume continues in; inputs: all must not hand it to a put as an artifact.
func TestAllArtifactsSkipsStagingLeftovers(t *testing.T) {
	t.Parallel()

	provider := stepCacheProvider(t, t.TempDir())
	bw := seededBuild(t, provider, "notes")

	leftover := stagingPath(filepath.Join(bw.(*isolatingBuild).artifacts, "repo"))

	err := os.MkdirAll(leftover, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	names, err := bw.(*isolatingBuild).allArtifacts()
	if err != nil {
		t.Fatal(err)
	}

	if len(names) != 1 || names[0] != "repo" {
		t.Errorf("allArtifacts = %v, want [repo]", names)
	}
}

func writeOneFileTree(t *testing.T, dir, name, contents string) {
	t.Helper()

	err := os.MkdirAll(dir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(dir, name), contents)
}

// An install that fails after the old tree was moved aside puts it back, and a
// successful one leaves no set-aside tree behind.
func TestSwapInKeepsTheOldTreeWhenTheInstallFails(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	dst := filepath.Join(parent, "artifact")
	writeOneFileTree(t, dst, "old.txt", "old")

	err := swapIn(filepath.Join(parent, "never-staged"), dst, copyBackend{}.remove)
	if err == nil {
		t.Fatal("installing a missing staging tree reported success")
	}

	if got := readFile(t, filepath.Join(dst, "old.txt")); got != "old" {
		t.Errorf("old tree = %q, want it restored", got)
	}

	staged := stagingPath(dst)
	writeOneFileTree(t, staged, "new.txt", "new")

	err = swapIn(staged, dst, copyBackend{}.remove)
	if err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, filepath.Join(dst, "new.txt")); got != "new" {
		t.Errorf("new tree = %q", got)
	}

	assertNoStagingLeft(t, parent)
}
