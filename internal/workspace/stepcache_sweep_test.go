package workspace

// Two gaps the mutation sweep found in the step cache's edges.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestValidateCachedNamesRefusesAMappedPathThatEscapes: an output_mapping
// value is a path, and one that leaves the store is refused even though
// the declared name it maps is fine.
func TestValidateCachedNamesRefusesAMappedPathThatEscapes(t *testing.T) {
	t.Parallel()

	err := validateCachedNames(StepCacheRequest{
		Outputs:       []string{"out"},
		OutputMapping: map[string]string{"out": "../escape"},
	})
	if err == nil {
		t.Fatal("a mapped output path that escapes the store was accepted")
	}

	err = validateCachedNames(StepCacheRequest{
		Outputs:       []string{"out"},
		OutputMapping: map[string]string{"out": "findings/alpha"},
	})
	if err != nil {
		t.Errorf("a collecting cell's mapped path was refused: %v", err)
	}
}

// TestDiscardStagedRemovesOnlyWhatWasNotCommitted: a committed output has
// its tmp cleared and must be left alone; an uncommitted one is removed.
func TestDiscardStagedRemovesOnlyWhatWasNotCommitted(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	cache, err := newStepCache(copyBackend{}, root, 10)
	if err != nil {
		t.Fatal(err)
	}

	leftover := filepath.Join(root, "out"+stagedSuffix)
	committed := filepath.Join(root, "done")

	for _, dir := range []string{leftover, committed} {
		err = os.MkdirAll(dir, 0o750)
		if err != nil {
			t.Fatal(err)
		}
	}

	cache.discardStaged([]stagedOutput{{tmp: leftover, dst: filepath.Join(root, "out")}, {tmp: "", dst: committed}})

	_, err = os.Stat(leftover)
	if err == nil {
		t.Error("an uncommitted staged output was left behind")
	}

	_, err = os.Stat(committed)
	if err != nil {
		t.Error("a committed output was removed by the discard")
	}
}
