package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// stepCacheProvider is a copy-strategy provider over a durable root, which is
// the only condition the step cache asks for.
func stepCacheProvider(t *testing.T, root string) Provider {
	t.Helper()

	provider, err := NewProvider(&config.WorkspaceConfig{Strategy: "copy", Root: root}, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	return provider
}

// seededBuild is a fresh build whose artifact store already holds one input
// artifact with the given content — what a get: would have left behind.
func seededBuild(t *testing.T, provider Provider, content string) BuildWorkspace {
	t.Helper()

	const input = "repo"

	bw, err := provider.NewBuild(context.Background(), "build")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = bw.Close() })

	dir, err := bw.ResourceDir(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "NOTES.txt"), []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return bw
}

// produceOutput runs the work a cacheable step would do: materialize a space,
// write into the declared output, capture it back.
func produceOutput(t *testing.T, bw BuildWorkspace, req StepCacheRequest, content string) {
	t.Helper()

	ctx := context.Background()

	space, err := bw.TaskSpace(ctx, "work", req.Inputs, req.Outputs, req.InputMapping, req.OutputMapping)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = space.Close() }()

	err = os.WriteFile(filepath.Join(space.Dir(), req.Outputs[0], "summary.md"), []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = space.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
}

func mustRestore(t *testing.T, bw BuildWorkspace, req StepCacheRequest) StepCacheResult {
	t.Helper()

	caching, ok := bw.(StepCaching)
	if !ok {
		t.Fatal("build does not implement StepCaching")
	}

	res, err := caching.RestoreStep(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	return res
}

func stepRequest() StepCacheRequest {
	return StepCacheRequest{ContentHash: "content-of-the-step", Inputs: []string{"repo"}, Outputs: []string{"report"}}
}

// TestStepCacheRoundTrip is the mechanism in one pass: a miss, the work, the
// store — and then a SECOND build finding it and getting the artifact back.
func TestStepCacheRoundTrip(t *testing.T) {
	t.Parallel()

	provider := stepCacheProvider(t, t.TempDir())
	req := stepRequest()

	first := seededBuild(t, provider, "notes")

	miss := mustRestore(t, first, req)
	if miss.Hit {
		t.Fatal("an empty cache reported a hit")
	}

	if miss.Key == "" {
		t.Fatal("a lookup against an empty cache returned no key to store under")
	}

	produceOutput(t, first, req, "the summary")

	caching, _ := first.(StepCaching)

	err := caching.StoreStep(context.Background(), miss.Key, req)
	if err != nil {
		t.Fatal(err)
	}

	// A different build, same input bytes: the same work, so the same key.
	second := seededBuild(t, provider, "notes")

	hit := mustRestore(t, second, req)
	if !hit.Hit {
		t.Fatal("the second build missed an entry the first one stored")
	}

	if hit.Key != miss.Key {
		t.Errorf("key changed between builds: %q then %q — the key is carrying plan position", miss.Key, hit.Key)
	}

	restored := filepath.Join(second.(*isolatingBuild).artifacts, "report", "summary.md")

	got, err := os.ReadFile(restored) //nolint:gosec // a t.TempDir()-scoped path the test arranged
	if err != nil {
		t.Fatalf("the restored output is not in the artifact store: %v", err)
	}

	if string(got) != "the summary" {
		t.Errorf("restored output = %q, want %q", got, "the summary")
	}
}

// TestStepCacheKeyFollowsInputBytes is the design's whole point: the step's
// declaration is identical, and only what its input HOLDS is different.
func TestStepCacheKeyFollowsInputBytes(t *testing.T) {
	t.Parallel()

	provider := stepCacheProvider(t, t.TempDir())
	req := stepRequest()

	before := mustRestore(t, seededBuild(t, provider, "notes"), req)
	after := mustRestore(t, seededBuild(t, provider, "different notes"), req)

	if before.Key == after.Key {
		t.Error("the key did not change when the input's content did")
	}
}

// TestStepCacheKeyFollowsStepContent: the other half of the key.
func TestStepCacheKeyFollowsStepContent(t *testing.T) {
	t.Parallel()

	provider := stepCacheProvider(t, t.TempDir())
	bw := seededBuild(t, provider, "notes")

	req := stepRequest()
	other := stepRequest()
	other.ContentHash = "a different command"

	if mustRestore(t, bw, req).Key == mustRestore(t, bw, other).Key {
		t.Error("two different steps over the same input share a key")
	}
}

// TestStepCacheIgnoresAPartialEntry: a store interrupted halfway must cost a
// re-run, never a step handed a half-populated set of inputs.
func TestStepCacheIgnoresAPartialEntry(t *testing.T) {
	t.Parallel()

	provider := stepCacheProvider(t, t.TempDir())

	req := stepRequest()
	req.Outputs = []string{"report", "coverage"}

	bw := seededBuild(t, provider, "notes")
	res := mustRestore(t, bw, req)

	// Store only the first of the two declared outputs, which is what an
	// interrupted store leaves behind.
	partial := req
	partial.Outputs = []string{"report"}

	produceOutput(t, bw, partial, "the summary")

	caching, _ := bw.(StepCaching)

	err := caching.StoreStep(context.Background(), res.Key, partial)
	if err != nil {
		t.Fatal(err)
	}

	if mustRestore(t, seededBuild(t, provider, "notes"), req).Hit {
		t.Error("an entry missing a declared output reported a hit")
	}
}

// TestStepCacheNeedsADurableRoot: with a provider-owned temp root there is
// nowhere for an entry to outlive the run, so there is no cache at all.
func TestStepCacheNeedsADurableRoot(t *testing.T) {
	t.Parallel()

	provider, err := NewProvider(&config.WorkspaceConfig{Strategy: "copy"}, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	bw := seededBuild(t, provider, "notes")

	if res := mustRestore(t, bw, stepRequest()); res.Hit || res.Key != "" {
		t.Errorf("a temp-root build offered a cache: %+v", res)
	}
}
