# 009 — `race:` speculative execution

**Source:** [robustness.md #10](../robustness.md#10-race--speculative-execution-first-success-wins)
**Tier:** 2 (agent-native) · **Priority rationale:** shares
[002](002-in-parallel-fan-out.md)'s branch runner with
[008](008-ensemble-agents.md) but is riskier (irreversible side effects from a
cancelled branch) and more niche (latency hedging, not the common case), so it
follows ensemble rather than leading the agent-native tier.

## Feature

Run a cheap/fast path and a reliable/slow path concurrently; keep whichever
succeeds first and cancel the other.

```yaml
- race:
    steps:
      - {agent: fast-model, prompt: "Summarize the release."}
      - {agent: strong-model, prompt: "Summarize the release."}
```

## Additional feature details

- **This is a latency hedge, not a cost saver.** Running both branches
  always costs both, even when the fast one usually wins — the value is
  "never wait for the slow path when the fast one is good enough," not
  cost reduction. Docs/examples should frame it this way explicitly so users
  don't reach for `race:` expecting to save money (they want
  [008](008-ensemble-agents.md)'s per-member caching, or plain retries, for
  that).
- **Losing branches must not have already mutated shared state.** If a
  losing branch already executed a `run_shell`/custom tool call with a
  real-world side effect (filed an issue, sent a notification) before being
  cancelled, that side effect doesn't roll back — cancellation only stops
  *future* work. `race:` should be documented as safe primarily for
  read/generate-only agents, and the feature should require `workspace:`
  isolation at load time (mirrors the doc's own note) so losing branches at
  least can't corrupt a shared filesystem view.
- **Output contract.** Branches should declare identical `outputs:` so
  downstream steps don't need to know which branch actually won — the
  winning branch's outputs become the step's outputs.
- **"First success" needs a precise definition.** Does a branch that returns
  quickly but with a low-confidence/degenerate result count as a win? For
  agent branches, "success" should mean "completed without model/tool
  error," not any judgment about output quality — quality gating belongs to
  a downstream `assert:`/`verdicts:` step, not to `race:` itself.
