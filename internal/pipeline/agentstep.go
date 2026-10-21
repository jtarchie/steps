package pipeline

// agent: — everything around an agent step's conversation: its key, the step
// cache, its node and its outputs. The conversation itself is internal/agent's.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/workspace"
)

// agentOutcome is what an agent step reports beyond its error: the node hash
// (on an error, the node the failure was recorded under — the caller must not
// chain on it, and it stays "" when the failure preceded any node), the
// verdict applyRouting keys on and its note, and the model's final response,
// which a later step's context: from: can read. response is set whenever a
// conversation ran, including on a failure.
type agentOutcome struct {
	hash     string
	verdict  string
	note     string
	response string
	// cached: the declared outputs were restored from an earlier run rather
	// than produced by a conversation, so no model was paid.
	cached bool
}

// executeAgentStep keys an agent step against parentHash (agent steps are
// never chain-skippable), reuses it when the step cache holds its outputs, and
// otherwise runs its conversation and records the outcome.
func executeAgentStep(ctx context.Context, r stepRunner, i int, step config.Step, parentHash string) (agentOutcome, error) {
	ctx, placed := withPlacementSink(ctx)

	out, err := runAgentStepUnrecorded(ctx, r, i, step, parentHash)

	// After its Close, and only under a node it recorded: a failure before the node has no row to reference, and it never ran anywhere either.
	if out.hash != "" {
		recordPlacement(ctx, r, placed, i, step.DisplayName(), out.hash, out.hash)
	}

	return out, err
}

func runAgentStepUnrecorded(ctx context.Context, r stepRunner, i int, step config.Step, parentHash string) (agentOutcome, error) {
	fail := func(err error) error { return fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err) }

	prepared, err := agent.PrepareStep(ctx, r.cfg, step, r.bw, r.st, placedAgentRunner(step))
	if err != nil {
		return agentOutcome{}, fail(err)
	}
	defer prepared.Close()

	content, err := agentStepContent(r.cfg, prepared)
	if err != nil {
		return agentOutcome{}, fail(err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindAgent, content, parentHash)
	if err != nil {
		return agentOutcome{}, fail(err)
	}

	slog.DebugContext(ctx, "job.step", "job", r.jobName, "index", i, "kind", "agent", "agent", step.Agent)

	// The name this step is KNOWN by: an across: cell reports and records under
	// its labelled identity rather than the agent it resolves through, so two
	// cells of one matrix are tellable apart in a run.
	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindAgent, StepIndex: i, Resource: step.DisplayName(), Content: content}

	// Keyed on the PREPARED step, like the hash: its cacheability and its
	// outputs are the resolved step's.
	req, err := agentCacheRequest(content, prepared.Step())
	if err != nil {
		return agentOutcome{}, fail(err)
	}

	// After preparing, because the key is not knowable before: a message_files:
	// naming a run-time artifact is only loaded once the step's workspace exists.
	// A hit therefore costs one materialized workspace and whatever tool servers
	// the grant starts — but not the tree probe, which waits for the
	// conversation (see agent's openTree), and not a model call.
	cached, err := reuseStep(ctx, r, prepared.Step(), req, node)
	if err != nil {
		return agentOutcome{}, fail(err)
	}

	if cached.Hit {
		return agentOutcome{hash: hash, cached: true}, nil
	}

	out, err := runAgentConversation(ctx, r, prepared, node, fail)
	if err != nil {
		return out, err
	}

	// After the node is recorded, so a run that could not record its own
	// outcome does not leave an entry behind claiming the work is done.
	workspace.SaveStepCache(ctx, r.bw, cached.Key, req)

	return out, nil
}

// agentStepContent is what an agent step is keyed on: its configuration as
// configured (the primary invocation, so a fallback firing is availability
// and not content), and what its memory: scope holds as it starts.
func agentStepContent(cfg *config.Config, prepared *agent.PreparedStep) (map[string]any, error) {
	content, err := merkle.AgentContentMap(cfg, prepared.Step(), prepared.Primary())
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	if digest := prepared.MemoryDigest(); digest != "" {
		content["memory_preload"] = digest
	}

	return content, nil
}

// runAgentConversation records the step's node as running, runs the
// conversation, captures its outputs and records how it ended.
func runAgentConversation(
	ctx context.Context, r stepRunner, prepared *agent.PreparedStep, node merkle.Node, fail func(error) error,
) (agentOutcome, error) {
	// The node is recorded BEFORE the conversation, as running, because the
	// usage and transcript rows Run writes reference it: agent_usage.node_hash
	// and node_transcripts.hash are foreign keys into nodes, which is what lets
	// retention delete a node and have its spend record and its transcript go
	// with it rather than survive as rows nothing can reach.
	//
	// Every path below re-records the node with its real status, since
	// RecordNode upserts. What a placeholder therefore leaves behind is a node
	// still marked running only when the PROCESS died mid-conversation — which
	// is true, and more than the nothing-at-all that was recorded before.
	//
	// It cannot be a cache hit in that state: HasNodeSucceeded asks for
	// succeeded, and job_runs — the chain-level index — is not written here.
	//
	// Best-effort, like the usage and transcript writes it exists to enable: a
	// bookkeeping row must never be what fails a step that would otherwise run.
	// A failure here is not silent — those two writes log their own warning when
	// the reference they need is missing — and the success path below records the
	// node again with a returned error.
	//
	// On a DETACHED context, matching the two writes it enables. Written on the
	// ambient ctx it was useless in the one case that matters: a step killed by
	// timeout: or Ctrl-C has a cancelled ctx, so the placeholder never landed,
	// and both later writes then failed the foreign keys they had just gained —
	// losing the tokens an aborted agent step spent and its transcript, on
	// precisely the runs worth investigating.
	bestEffort(ctx, "RecordNode", r.st.RecordNode(context.WithoutCancel(ctx), nodeRecord(node), r.jobName, "running", nil, nil))

	res, err := prepared.Run(ctx, agent.StepRun{JobName: r.jobName, Index: node.StepIndex, Name: node.Resource, NodeHash: node.Hash})
	if err != nil {
		recordStepFailureWith(ctx, r, node, res.Record, err)

		return agentOutcome{hash: node.Hash, response: res.Response}, fail(err)
	}

	err = prepared.Capture(ctx)
	if err == nil {
		err = r.st.RecordNode(ctx, nodeRecord(node), r.jobName, "succeeded", res.Record, nil)
	}

	if err != nil {
		wrapped := fail(err)
		// Detached, so the node the row is published under does not stay
		// "running" when a cancelled ctx is why the success write failed.
		recordStepFailureWith(ctx, r, node, res.Record, wrapped)

		return agentOutcome{hash: node.Hash, response: res.Response}, wrapped
	}

	return agentOutcome{hash: node.Hash, verdict: res.Verdict, note: res.Note, response: res.Response}, nil
}
