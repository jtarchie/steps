package pipeline

import (
	"slices"
	"testing"

	"github.com/jtarchie/steps/internal/merkle"
)

// TestReplayedHashesNameNoOtherBuildsNode: a skip point shared by two input
// sets, with a get after it binding a different version in each, has one
// node per set at that position — a replayed row naming either would name a
// build it is not.
func TestReplayedHashesNameNoOtherBuildsNode(t *testing.T) {
	t.Parallel()

	chain := func(hashes ...string) merkle.Chain {
		nodes := make([]merkle.Node, 0, len(hashes))
		for _, hash := range hashes {
			nodes = append(nodes, merkle.Node{Hash: hash})
		}

		return merkle.Chain{Nodes: nodes}
	}

	chains := chainHashes([]merkle.Chain{
		chain("prep", "get-v1", "test"),
		chain("prep", "get-v2", "test"),
		chain("other", "unrelated"),
	})

	got := replayedHashes(chains, "prep", 2)
	if want := []string{"", "test"}; !slices.Equal(got, want) {
		t.Errorf("replayedHashes = %q, want %q", got, want)
	}

	if got := replayedHashes(chains, "", 2); !slices.Equal(got, []string{"", ""}) {
		t.Errorf("an unnamed skip point replayed as %q, want nothing named", got)
	}
}
