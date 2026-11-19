package workspace

import (
	"context"
	"errors"
	"testing"
)

var errPulled = errors.New("the tree was pulled home")

// fakeHolds answers Claim with claim and refuses every Pull, so a test fails loudly if a cache brings a held tree home.
func fakeHolds(claims *int, claim *error) Holds {
	return Holds{
		Pull: func(string, string) func(context.Context, string) error {
			return func(context.Context, string) error { return errPulled }
		},
		Claim: func(context.Context, string, string) error {
			*claims++

			return *claim
		},
	}
}

func heldBuild(t *testing.T, provider Provider) *isolatingBuild {
	t.Helper()

	bw, err := provider.NewBuild(t.Context(), "build")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = bw.Close() })

	build, ok := bw.(*isolatingBuild)
	if !ok {
		t.Fatalf("NewBuild = %T", bw)
	}

	return build
}

// storeHeldOutput runs a step whose output docker holds through the cache's miss and store, and returns the entry's trees.
func storeHeldOutput(t *testing.T, provider Provider, req StepCacheRequest, holds Holds) map[string]CachedTree {
	t.Helper()

	ctx := WithHolds(t.Context(), holds)

	first := heldBuild(t, provider)

	err := first.HoldRemote("built", RemoteArtifact{Name: "out", Digest: "d1", Holder: "local:", Pull: holds.Pull("local:", "d1")})
	if err != nil {
		t.Fatal(err)
	}

	miss := LookupStepCache(ctx, first, req)
	if miss.Hit || miss.Key == "" {
		t.Fatalf("first lookup = %+v, want a miss with a key", miss)
	}

	err = first.StoreStep(ctx, miss.Key, req)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	trees, err := first.stepCache.index.CacheEntry(ctx, kindStep, miss.Key)
	if err != nil {
		t.Fatal(err)
	}

	return trees
}

// TestTheStepCacheFilesAHeldOutputWhereDockerHoldsIt: the entry records the holder and digest, never the bytes, and a hit claims the tree and hands it on held.
func TestTheStepCacheFilesAHeldOutputWhereDockerHoldsIt(t *testing.T) {
	t.Parallel()

	var (
		claims int
		claim  error
	)

	holds := fakeHolds(&claims, &claim)
	ctx := WithHolds(context.Background(), holds)
	provider := stepCacheProvider(t, t.TempDir())
	req := StepCacheRequest{ContentHash: "work", Outputs: []string{"out"}, OutputMapping: map[string]string{"out": "built"}}

	trees := storeHeldOutput(t, provider, req, holds)

	if trees["out"].Holder == "" {
		t.Errorf("the entry names %+v; it should name where docker holds the output, not this machine's store", trees["out"])
	}

	second := heldBuild(t, provider)

	hit := LookupStepCache(ctx, second, req)
	if !hit.Hit || claims != 1 {
		t.Fatalf("second lookup = %+v after %d claims, want a hit that claimed the tree once", hit, claims)
	}

	remote, held := second.remoteArtifact("built")
	if !held || remote.Digest != "d1" || remote.Holder != "local:" || remote.Name != "out" {
		t.Errorf("the restored output is held as %+v (%v), want d1 on local: filed as out", remote, held)
	}
}

// TestAHeldStepCacheEntryIsAMissWithoutItsTree: a holder that no longer has the tree, or no way to reach docker at all, is a miss rather than an input nobody holds.
func TestAHeldStepCacheEntryIsAMissWithoutItsTree(t *testing.T) {
	t.Parallel()

	var (
		claims int
		claim  error
	)

	holds := fakeHolds(&claims, &claim)
	ctx := WithHolds(context.Background(), holds)
	provider := stepCacheProvider(t, t.TempDir())
	req := StepCacheRequest{ContentHash: "work", Outputs: []string{"out"}, OutputMapping: map[string]string{"out": "built"}}

	storeHeldOutput(t, provider, req, holds)

	claim = errors.New("evicted")

	if gone := LookupStepCache(ctx, heldBuild(t, provider), req); gone.Hit {
		t.Error("an entry whose tree the holder no longer has was a hit")
	}

	if gone := LookupStepCache(context.Background(), heldBuild(t, provider), req); gone.Hit {
		t.Error("an entry naming a held tree was a hit with no way to reach docker")
	}
}

// TestTheResourceCacheFilesAHeldTreeWhereDockerHoldsIt is the same for a get: the fetch runs once, and the second build is handed the held tree.
func TestTheResourceCacheFilesAHeldTreeWhereDockerHoldsIt(t *testing.T) {
	t.Parallel()

	var (
		claims int
		claim  error
	)

	ctx := WithHolds(context.Background(), fakeHolds(&claims, &claim))
	provider := stepCacheProvider(t, t.TempDir())
	fetches := 0

	fetchInto := func(build *isolatingBuild) func(string) error {
		return func(string) error {
			fetches++

			return build.HoldRemote("src", RemoteArtifact{Name: "src", Digest: "d2", Holder: "local:", Pull: fakeHolds(&claims, &claim).Pull("local:", "d2")})
		}
	}

	first := heldBuild(t, provider)

	_, err := first.FetchResource(ctx, "src", "key", fetchInto(first))
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	second := heldBuild(t, provider)

	_, err = second.FetchResource(ctx, "src", "key", fetchInto(second))
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}

	if fetches != 1 || claims != 1 {
		t.Errorf("%d fetches and %d claims, want the second build handed the held tree: 1 and 1", fetches, claims)
	}

	if remote, held := second.remoteArtifact("src"); !held || remote.Digest != "d2" {
		t.Errorf("the second build holds src as %+v (%v), want d2", remote, held)
	}

	claim = errors.New("evicted")
	third := heldBuild(t, provider)

	_, err = third.FetchResource(ctx, "src", "key", fetchInto(third))
	if err != nil || fetches != 2 {
		t.Errorf("an evicted held tree: err %v after %d fetches, want it fetched again", err, fetches)
	}
}
