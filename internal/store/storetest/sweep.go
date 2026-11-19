package storetest

import (
	"encoding/json"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// ContentProbe reaches past the interface into node_content, which no store method lists: interned content is only ever read back through a node, so a row no node names is invisible to the contract.
type ContentProbe struct {
	// Plant inserts a node_content row under contentHash that no node names.
	Plant func(t *testing.T, st store.Store, contentHash string)
	// Rows counts the node_content rows keyed contentHash.
	Rows func(t *testing.T, st store.Store, contentHash string) int
}

// RunContentSweep holds a driver to the narrowed node_content sweep: a prune looks only at the content of the nodes it deleted, while Delete still sweeps the whole table.
func RunContentSweep(t *testing.T, open OpenFunc, probe ContentProbe) {
	t.Helper()

	opened := func(name string) store.Store {
		st := open(t, name)
		t.Cleanup(func() { _ = st.Close() })

		return st
	}

	ctx := ctxFor(t)
	mine := opened("mine")
	other := opened("other")

	shared := recordContent(t, mine, hashOf(1), map[string]any{"shared": true})
	recordContent(t, other, hashOf(1), map[string]any{"shared": true})
	freed := recordContent(t, mine, hashOf(2), map[string]any{"freed": true})

	for n := range store.NodesPerRetainedRun - 2 {
		recordContent(t, mine, hashOf(n+3), map[string]any{"own": n})
	}

	recordContent(t, mine, hashOf(1_000), map[string]any{"newest": true})
	recordContent(t, mine, hashOf(1_001), map[string]any{"newest": 2})

	const stray = "stray"

	probe.Plant(t, mine, stray)

	err := mine.Prune(ctx, store.Retention{JobName: "build", Runs: 1}, "")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	nodes, err := mine.ListNodes(ctx, "build", 0)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}

	if len(nodes) != store.NodesPerRetainedRun {
		t.Fatalf("%d nodes survived, want %d: the prune did not reap the two oldest", len(nodes), store.NodesPerRetainedRun)
	}

	if got := probe.Rows(t, mine, shared); got != 1 {
		t.Errorf("%d shared content rows, want 1: a sweep that ignores the other pipeline's reference deleted it", got)
	}

	if got := probe.Rows(t, mine, freed); got != 0 {
		t.Errorf("%d freed content rows, want 0: the candidate nobody references was not swept", got)
	}

	if got := probe.Rows(t, mine, stray); got != 1 {
		t.Errorf("Prune swept a stray row it never freed; only Delete sweeps the whole table")
	}

	err = other.Delete(ctx)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if got := probe.Rows(t, mine, stray); got != 0 {
		t.Errorf("%d stray rows after Delete, want 0: its sweep must stay table-wide", got)
	}
}

// recordContent records a node of job build and returns the key the driver interned its content under.
func recordContent(t *testing.T, st store.Store, hash string, content map[string]any) string {
	t.Helper()

	err := st.RecordNode(ctxFor(t), store.NodeRecord{Hash: hash, Kind: "task", Resource: "step", Content: content}, "build", "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode %s: %v", hash, err)
	}

	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}

	return store.ContentKey(encoded)
}
