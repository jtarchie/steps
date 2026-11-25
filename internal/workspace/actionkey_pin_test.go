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

	const want = "f205d374f9562e7e7b184d278f1d86a3cdb79a6dd7ecc168a02e9c692cd5b325"
	if key != want {
		t.Errorf("action key = %s, want %s", key, want)
	}
}
