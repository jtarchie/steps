package store

// The columns where NULL and a value mean different things.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecordNodeStoresWhatItWasGiven covers three inversions mutation testing
// found nothing asserting: whether a parent hash is stored or nulled, whether
// a step's result is marshalled at all, and whether the marshalled bytes are
// stored or nulled.
//
// Each of the three is a one-character change — == for !=, != for == — that
// swaps a value for NULL or NULL for a value, and every one of them survived
// the whole suite. The reason they matter is the same reason nodes.parent_hash
// is a deliberate non-foreign-key: the chain is what the cache is read back
// along, and a node whose parent is NULL is a chain that starts over.
func TestRecordNodeStoresWhatItWasGiven(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	root := strings.Repeat("a", 64)
	child := strings.Repeat("b", 64)

	err := store.RecordNode(ctx, NodeRecord{
		Hash: root, Kind: "get", StepIndex: 0, Resource: "mentions",
		Content: map[string]any{"body": "the first step of the chain"},
	}, "build", "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode root: %v", err)
	}

	err = store.RecordNode(ctx, NodeRecord{
		Hash: child, ParentHash: root, Kind: "task", StepIndex: 1, Resource: "compile",
		Content: map[string]any{"body": "the second"},
	}, "build", "succeeded", map[string]any{"output": "built"}, nil)
	if err != nil {
		t.Fatalf("RecordNode child: %v", err)
	}

	rows, err := store.NodesByHash(ctx, []string{root, child})
	if err != nil {
		t.Fatalf("NodesByHash: %v", err)
	}

	// The chain, which is the whole reason the column exists.
	if got := rows[child].ParentHash; got != root {
		t.Errorf("child's parent = %q, want the root hash — the chain does not link", got)
	}

	// And a root really has none, so the two are distinguishable. Empty here
	// is the NULL sqlite stores; a hash would mean a chain that loops.
	if got := rows[root].ParentHash; got != "" {
		t.Errorf("root's parent = %q, want nothing", got)
	}

	// A result that was produced is a result that is readable: this is what a
	// cache HIT replays instead of re-running the step.
	if got := rows[child].Result; !strings.Contains(got, "built") {
		t.Errorf("child's result = %q, want what the step produced", got)
	}

	if got := rows[root].Result; got != "" {
		t.Errorf("root's result = %q, want nothing — it produced none", got)
	}
}
