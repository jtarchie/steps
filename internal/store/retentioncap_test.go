package store

// The cache the count cap is supposed to CARRY.

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
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
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	for build := 1; build <= builds; build++ {
		syntheticBuild(ctx, t, store, "job", build)

		// Chains of their own, which syntheticBuild does not record: job_runs
		// holds one row per whole chain, and without any the cap on it is a
		// statement about an empty table.
		for chain := range chainsPerBuild {
			err := store.RecordJobRun(ctx, "job",
				fmt.Sprintf("%064x", build*1000+chain), "succeeded", nil)
			if err != nil {
				t.Fatalf("RecordJobRun: %v", err)
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

	err := store.PruneRuns(ctx, "job", keep, "")
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}

	if got := countRows(ctx, t, store, "nodes"); got != keep*nodesPerRetainedRun {
		t.Errorf("nodes = %d after a prune to %d runs, want %d — the cap is a MULTIPLE of run_history, and carrying less than it allows re-runs work that was cached",
			got, keep, keep*nodesPerRetainedRun)
	}

	if got := countRows(ctx, t, store, "job_runs"); got != keep*chainsPerRetainedRun {
		t.Errorf("job_runs = %d after a prune to %d runs, want %d — a reaped chain index re-runs a whole job that already went green",
			got, keep, keep*chainsPerRetainedRun)
	}
}

// TestVersionHistoryZeroMeansNoLimit pins the convention the comment above
// the branch already states and nothing checked.
//
// `version_history: 0` means no limit, the same convention every other cap in
// this repo uses (docs/attempts-timeout.md). The branch that enforces it is
// `if limit < 0`, and widening it to `limit <= 0` restores exactly the bug the
// comment records — zero silently becoming the 1000 default — with the whole
// suite still green. A cap nobody asked for is not a visible failure: it looks
// like a working pipeline until a resource passes a thousand versions and the
// oldest start disappearing.
func TestVersionHistoryZeroMeansNoLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	// Filed across several checks, not one. A prune never touches versions the
	// CURRENT check just reported — minReportedOrder is the floor — so a
	// single call of everything is protected in full and would pass under any
	// cap at all. The cap only bites on versions an EARLIER check filed.
	const (
		perCheck = 200
		checks   = 6
		filed    = perCheck * checks
	)

	for check := range checks {
		versions := make([]map[string]any, 0, perCheck)
		for i := range perCheck {
			versions = append(versions, map[string]any{"ref": fmt.Sprintf("v%04d", check*perCheck+i)})
		}

		_, err := store.RecordVersions(ctx, "mentions", versions, 0)
		if err != nil {
			t.Fatalf("RecordVersions check %d: %v", check, err)
		}
	}

	if got := countRows(ctx, t, store, "resource_versions"); got != filed {
		t.Errorf("kept %d of %d versions under version_history: 0, want all of them — zero means no limit, not the default cap of %d",
			got, filed, DefaultResourceVersionCap)
	}
}
