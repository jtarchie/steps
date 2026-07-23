# 008 — Ensemble agents: parallel fan-out, verdict fan-in

**Source:** [robustness.md #9](../robustness.md#9-ensemble-agents--parallel-fan-out-verdict-fan-in)
**Tier:** 2 (agent-native) · **Priority rationale:** first of the
no-Concourse-equivalent features, and the most directly valuable — it composes
[002](002-in-parallel-fan-out.md)'s branch runner with the existing verdict
loop and handoff-context renderer with no new execution machinery, and
addresses a real, common failure mode (single-model blind spots).

## Feature

Run N agents on the same prompt in parallel, reduce their verdicts to one
outcome, and route on the reduced result.

```yaml
- ensemble:
    agents:
      - {agent: reviewer-a, prompt: "Review the diff for correctness."}
      - {agent: reviewer-b, prompt: "Review the diff for correctness."}
      - {agent: reviewer-c, prompt: "Review the diff for correctness."}
    verdicts: [approve, reject]
    decide: majority        # or: unanimous, any, or an agent name to judge
  to:
    approve: publish
    reject: revise
```

## Additional feature details

- **Cost is N×, and that needs to be visible.** Running three agents costs
  three times what one does. This should be documented plainly next to the
  feature (not buried), and pairs naturally with
  [010](010-budgets.md) — an ensemble is exactly the kind of step where a
  per-job budget cap matters most.
- **Tie-breaking.** `decide: majority` needs an explicit rule for a tie
  (even N, or all-different verdicts with no majority) — silently picking
  the first verdict would be an invisible bug. Recommend: a tie is itself an
  error unless a fallback judge agent is named, so the pipeline author is
  forced to decide the policy rather than getting undefined behavior.
- **Member failure vs. member verdict.** If one ensemble member errors
  (model/tool failure) rather than producing a verdict, it must not silently
  count as a vote either way — the pipeline author needs a policy: exclude
  it and decide among the rest, or fail the whole ensemble. This is
  materially different from a "reject" verdict and should be distinguishable
  in the `decide:` logic.
- **Judge-agent transparency.** When `decide:` names an agent instead of a
  reduction rule, that judge's own reasoning (why it picked the verdict it
  did) should be recorded and inspectable the same way a normal agent run
  is — otherwise the ensemble becomes a black box one level deeper than a
  single agent already is.
- **Each member is independently cacheable.** Since each member is its own
  merkle node, an ensemble re-run after a prompt tweak to only one member
  should only re-run that member, not all three — worth stating as an
  explicit expectation since it's a real cost-saving property, not just an
  implementation detail.
