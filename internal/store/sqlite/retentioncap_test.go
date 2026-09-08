package sqlite

// The cache the count cap is supposed to CARRY.

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestRetentionCarriesTheCacheItsCapAllows is the lower bound nothing asserted.
//
// nodes and job_runs are the merkle and chain caches, and they are capped as a
// MULTIPLE of run_history: — deliberately, so a working pipeline keeps enough
// history to go on skipping work it has already done. Every existing
// assertion is an upper bound: the footprint test proves the cap BINDS, which
// a multiplier of zero satisfies perfectly. So mutating `limit *
// nodesPerRetainedRun` into `limit / nodesPerRetainedRun` — a cap of nothing,
// every cached node reaped on the next build, every step re-run — survived
// the whole suite.
//
// Asserted as an exact count rather than a floor, because a floor is killed by
// division and not by addition, and both are one character.
func TestRetentionCarriesTheCacheItsCapAllows(t *testing.T) {
	t.Parallel()

	const (
		builds         = 12
		keep           = 3
		chainsPerBuild = 3
		nodesPerBuild  = 6 // len(syntheticPlan)
	)

	ctx := context.Background()
	st := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = st.Close() }()

	for build := 1; build <= builds; build++ {
		syntheticBuild(ctx, t, st, "job", build)

		// Chains of their own, which syntheticBuild does not record: job_runs
		// holds one row per whole chain, and without any the cap on it is a
		// statement about an empty table.
		for chain := range chainsPerBuild {
			err := st.RecordChainSucceeded(ctx, "job", fmt.Sprintf("%064x", build*1000+chain))
			if err != nil {
				t.Fatalf("RecordChainSucceeded: %v", err)
			}
		}
	}

	// Both tables must actually be over their cap, or this proves nothing: an
	// uncapped table and a correctly capped one look identical below the line.
	if builds*nodesPerBuild <= keep*nodesPerRetainedRun {
		t.Fatalf("the fixture never exceeds the node cap (%d <= %d)", builds*nodesPerBuild, keep*nodesPerRetainedRun)
	}

	if builds*chainsPerBuild <= keep*chainsPerRetainedRun {
		t.Fatalf("the fixture never exceeds the chain cap (%d <= %d)", builds*chainsPerBuild, keep*chainsPerRetainedRun)
	}

	err := st.Prune(ctx, store.Retention{JobName: "job", Runs: keep}, "")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if got := countRows(ctx, t, st, "nodes"); got != keep*nodesPerRetainedRun {
		t.Errorf("nodes = %d after a prune to %d runs, want %d — the cap is a MULTIPLE of run_history, and carrying less than it allows re-runs work that was cached",
			got, keep, keep*nodesPerRetainedRun)
	}

	if got := countRows(ctx, t, st, "job_runs"); got != keep*chainsPerRetainedRun {
		t.Errorf("job_runs = %d after a prune to %d runs, want %d — a reaped chain index re-runs a whole job that already went green",
			got, keep, keep*chainsPerRetainedRun)
	}
}
