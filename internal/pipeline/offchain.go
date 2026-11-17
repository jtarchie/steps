package pipeline

import (
	"context"

	"github.com/jtarchie/steps/internal/store"
)

// offChainStore is the store a block hook body runs against. A hook is
// deliberately outside the merkle chain — never skipped for having succeeded
// before — and a block body reuses the plan's runners, which record nodes,
// ask the store what already succeeded, and key placements and spend on node
// hashes. Interposing here keeps that one seam rather than a flag every runner
// must remember: nothing a body runs records a node, every "has this
// succeeded" answers no, and what still records (placements, agent spend) is
// filed the way a hook's own is, under no node.
//
// The "succeeded" answers matter even though the body records nothing: a body
// chains from no parent, exactly as a plan's first step does, so a hook cell
// and a plan cell with the same content share a hash.
type offChainStore struct {
	store.Store

	// slot is the hook scope's label, prefixed onto a placement's slot: a
	// node hash is not unique in a run once two hooks hold the same body.
	slot string
}

func (offChainStore) RecordNode(context.Context, store.NodeRecord, string, store.NodeStatus, map[string]any, error) error {
	return nil
}

func (offChainStore) HasNodeSucceeded(context.Context, string, string) (bool, error) {
	return false, nil
}

func (offChainStore) HasNodesSucceeded(context.Context, string, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// SaveNodeTranscript has no node to hang on: node_transcripts references nodes.
func (offChainStore) SaveNodeTranscript(context.Context, string, string) error { return nil }

func (s offChainStore) RecordPlacement(ctx context.Context, placement store.Placement) error {
	placement.Slot = s.slot + " / " + placement.Slot
	placement.NodeHash = ""

	return s.Store.RecordPlacement(ctx, placement) //nolint:wrapcheck // a pass-through
}

func (s offChainStore) RecordAgentUsage(ctx context.Context, usage store.AgentUsage) error {
	usage.NodeHash = ""
	usage.Kind = store.UsageHook

	return s.Store.RecordAgentUsage(ctx, usage) //nolint:wrapcheck // a pass-through
}

// offChain reports a runner inside a block hook body.
func (r stepRunner) offChain() bool {
	_, ok := r.st.(offChainStore)

	return ok
}
