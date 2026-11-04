package tree

import (
	"path/filepath"
	"testing"
)

// A search root that cannot be read is an empty result, not an error and not a crash: WalkDir hands the callback no entry at all for a missing root.
func TestSearchOfAMissingRootFindsNothing(t *testing.T) {
	t.Parallel()

	result, err := searchWalk(filepath.Join(t.TempDir(), "does-not-exist"), SearchOpts{Mode: "files", HeadLimit: 10})
	if err != nil {
		t.Fatalf("searchWalk over a missing root = %v, want no error", err)
	}

	if result.Total != 0 || len(result.Files) != 0 {
		t.Errorf("searchWalk over a missing root = %+v, want nothing found", result)
	}
}
