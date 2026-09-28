package pipeline

// The chain-skip index: which planned node hashes a prior run already covered.

import (
	"context"
	"fmt"
	"slices"

	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/store"
)

// computeChainSkippable reports, per chain, whether it's already covered by a
// prior succeeded job_runs row — batched into one query (per
// Store.HasSucceededBatch) instead of one per chain. An Unskippable chain is
// never even asked about; it stays false.
func computeChainSkippable(ctx context.Context, st store.Store, jobName string, chains []merkle.Chain) ([]bool, error) {
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

// replayedHashes is the node each of the count plan steps after skipped holds
// in the chains through it. A chain has one node per plan step, so position
// maps to position. A step is "" where the chains through skipped disagree —
// a later get fanning out binds a different version in each — so a row never
// names a node another build ran.
func replayedHashes(chains [][]string, skipped string, count int) []string {
	hashes := make([]string, count)
	if skipped == "" {
		return hashes
	}

	seen := false

	for _, chain := range chains {
		at := slices.Index(chain, skipped)
		if at < 0 || len(chain)-at-1 != count {
			continue
		}

		for offset, hash := range chain[at+1:] {
			switch {
			case !seen:
				hashes[offset] = hash
			case hashes[offset] != hash:
				hashes[offset] = ""
			}
		}

		seen = true
	}

	return hashes
}

// chainHashes keeps only the node hashes of chains: the walk holds them for
// its whole life, and a node's content map is dead weight there.
func chainHashes(chains []merkle.Chain) [][]string {
	out := make([][]string, len(chains))

	for i, chain := range chains {
		out[i] = make([]string, len(chain.Nodes))
		for j, node := range chain.Nodes {
			out[i][j] = node.Hash
		}
	}

	return out
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
func buildSkippableIndex(ctx context.Context, st store.Store, jobName string, chains []merkle.Chain) (map[string]bool, error) {
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
