package workspace

import "testing"

// TestActionKeyIsPinned: a change to the tree digest or the key's framing silently strands every cache entry, so it has to show up here and bump stepCacheDomain with it.
func TestActionKeyIsPinned(t *testing.T) {
	t.Parallel()

	root := writeTree(t, map[string]string{"a.txt": "alpha", "nested/b.txt": "beta"})

	key, err := (&stepCache{}).actionKey(func(string) (string, error) { return digestTree(root) }, StepCacheRequest{ContentHash: "content", Inputs: []string{"src"}})
	if err != nil {
		t.Fatal(err)
	}

	const want = "81746560a4c010165e4f32c15c9f324c1b090dc53b98f544a0d1c98a56db96cc"
	if key != want {
		t.Errorf("action key = %s, want %s", key, want)
	}
}
