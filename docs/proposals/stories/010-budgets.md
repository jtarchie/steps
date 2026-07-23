# 010 — Budgets: token/cost/wall-clock caps

**Source:** [robustness.md #11](../robustness.md#11-budgets--tokencost-wall-clock-caps-with-graceful-degradation)
**Tier:** 2 (agent-native) · **Priority rationale:** the natural next step
after [008](008-ensemble-agents.md)/[009](009-race-speculative-execution.md)
introduce multiplied model spend — a budget cap is the safety valve those two
features make more urgent, so it follows them directly.

## Feature

The agent analogue of `timeout:` — caps on token usage or cost, at the agent
or job level, with optional graceful degradation instead of a hard failure.

```yaml
agents:
  - name: writer
    source: {...}
    budget:
      tokens: 200000        # per invocation
jobs:
  - name: publish
    budget:
      cost: "$2.50"         # whole-job ceiling, all agent steps combined
    plan: [...]
```

## Additional feature details

- **Reporting should exist independent of enforcement.** Even before any cap
  is set, being able to see "this job spent 340K tokens / $1.80 across 4
  agent steps" after a run is valuable on its own — recommend shipping usage
  *reporting* as the first slice, with enforcement (`budget:`) as a second
  slice on top, since reporting has no correctness risk and immediately
  informs what caps are even reasonable to set.
- **Cost requires a maintained price table.** Token-based budgets are
  provider-agnostic and easy; `cost: "$2.50"` requires per-model pricing data
  that goes stale as providers change rates. This is an ongoing maintenance
  burden, not a one-time implementation cost — worth flagging explicitly so
  the decision to support `cost:` (vs. tokens-only) is made with that
  tradeoff in view. Recommend tokens-only for v1, cost as a clearly-labeled
  best-effort follow-up.
- **Degradation path.** `on_budget: {agent: cheaper-writer}` (swap to a
  cheaper agent rather than failing outright) is called out in the source
  doc as a natural extension — worth deciding whether it ships in v1 or is a
  fast-follow, since it changes the failure semantics (a budget breach
  becomes a routing event, not necessarily an `errored` classification).
- **Job budget attribution.** When a whole-job budget trips partway through
  a plan, which step "caused" it needs to be clear in output — the breach is
  cumulative across possibly-several agent steps, so pointing at only the
  step that pushed it over the line could be misleading without showing the
  running total.
- **Classification.** A blown budget should classify as `errored` (distinct
  from the model producing a bad/refused response, which is a different kind
  of failure) so `on_error` hooks — not `on_failure` — are what fire,
  consistent with how [000](000-attempts-and-timeout.md) treats timeouts.
- **Not hashed.** Like `assert:` and `timeout:`, a budget is an operational
  limit — adding one to an existing agent step must not invalidate its
  cache.
