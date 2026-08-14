package pipeline

// The chain-skip index: which planned node hashes a prior run already covered.

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/store"
)

// computeChainSkippable reports, per chain, whether it's already covered by a
// prior succeeded job_runs row — batched into one query (per
// Store.HasSucceededBatch) instead of one per chain. An Unskippable chain is
// never even asked about; it stays false.
func computeChainSkippable(ctx context.Context, st *store.Store, jobName string, chains []merkle.Chain) ([]bool, error) {
	toCheck := make([]string, 0, len(chains))

	for _, chain := range chains {
		if !chain.Unskippable {
			toCheck = append(toCheck, chain.RootHash)
		}
	}

	succeeded, err := st.HasSucceededBatch(ctx, jobName, toCheck)
	if err != nil {
		return nil, fmt.Errorf("has succeeded batch: %w", err)
	}

	chainSkippable := make([]bool, len(chains))

	for i, chain := range chains {
		if !chain.Unskippable {
			chainSkippable[i] = succeeded[chain.RootHash]
		}
	}

	return chainSkippable, nil
}

// buildSkippableIndex returns, for every node hash reachable across chains,
// whether every leaf merkle.Chain passing through it is already covered by a
// prior succeeded job_runs row. Any Unskippable chain (contains a put or
// agent step) is forced non-skippable everywhere along it — those steps
// (and everything feeding them) must always run. A node hash shared by
// multiple chains is skippable only if ALL chains through it are skippable
// (AND-rollup), which correctly forces get/task ancestors of an
// unskippable branch to execute even if a sibling branch is independently
// skippable.
func buildSkippableIndex(ctx context.Context, st *store.Store, jobName string, chains []merkle.Chain) (map[string]bool, error) {
	chainSkippable, err := computeChainSkippable(ctx, st, jobName, chains)
	if err != nil {
		return nil, fmt.Errorf("job %q: %w", jobName, err)
	}

	skippable := map[string]bool{}

	for i, chain := range chains {
		for _, node := range chain.Nodes {
			if prior, seen := skippable[node.Hash]; seen {
				skippable[node.Hash] = prior && chainSkippable[i]
			} else {
				skippable[node.Hash] = chainSkippable[i]
			}
		}
	}

	return skippable, nil
}
