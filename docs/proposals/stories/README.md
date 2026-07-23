# Robustness feature stories

Each feature from [../robustness.md](../robustness.md) broken into its own
story, numbered in recommended build order. Earlier stories are smaller and/or
unblock later ones; see each file's "Priority rationale" for why it sits where
it does.

## Tier 1 — Concourse parity gaps

| # | Story | What it closes |
|---|-------|-----------------|
| [000](000-attempts-and-timeout.md) | `attempts:`/`timeout:` on every step | No retry/timeout outside agent steps |
| [001](001-try-tolerated-failure.md) | `try:` tolerated failure | No way to shrug at a best-effort step's failure |
| [002](002-in-parallel-fan-out.md) | `in_parallel` fan-out | Nothing in a plan runs concurrently |
| [003](003-passed-cross-job-fan-in.md) | `passed:` cross-job fan-in | `watch` can trigger downstream on an unvetted version |
| [004](004-serial-groups.md) | `serial:`/`serial_groups:` | `--max-concurrent` can double-run or race a job |
| [005](005-across-matrix-fan-out.md) | `across:` matrix fan-out | No per-cell matrix execution/caching |
| [006](006-pipeline-vars.md) | `((var))` + `load_var` | No separation of pipeline shape from parameters |
| [007](007-webhook-triggered-checks.md) | Webhook-triggered checks | Only polling, no push-triggered checks |

## Tier 2 — agent-native (no Concourse equivalent)

| # | Story | What it adds |
|---|-------|----------------|
| [008](008-ensemble-agents.md) | Ensemble agents | Parallel multi-agent review with verdict fan-in |
| [009](009-race-speculative-execution.md) | `race:` speculative execution | First-success-wins latency hedge |
| [010](010-budgets.md) | Budgets | Token/cost caps, the agent analogue of `timeout:` |
| [011](011-approval-steps.md) | Approval steps | Human gate before an irreversible step |
| [012](012-agent-source-failover.md) | Agent source failover | Provider outage resilience |
| [013](013-watch-circuit-breaker.md) | Watch circuit breaker | Stop retriggering a job that's consistently broken |

## Cross-cutting notes surfaced while breaking these out

- **000/001/010 share an outcome-classification requirement**: timeouts and
  budget breaches should classify as `errored`, not `failed`, so `on_error`
  hooks (not `on_failure`) fire — consistent with existing `outcome.Classify`
  semantics.
- **002 is the keystone**: 005, 008, and 009 all reuse its branch-runner and
  cancellation semantics rather than inventing their own.
- **006 and 010 both touch secret handling**: a `((var))` sourced from a
  vars-file and a cost-budget's spend total both risk landing sensitive or
  operational data in `state.db` if not deliberately excluded from hashing —
  worth reviewing together against the existing `api_key_env`/`endpoint:`
  precedent in CLAUDE.md's trust-boundary notes.
- **007, 011, and 013 all need an outbound notification path** (webhook
  received, approval pending, breaker tripped) — likely worth designing once
  as a shared "notify" mechanism (e.g. a hook-triggered `put`) rather than
  three bespoke ones.
