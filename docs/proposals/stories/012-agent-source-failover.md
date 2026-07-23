# 012 — Agent source failover

**Source:** [robustness.md #13](../robustness.md#13-agent-source-failover)
**Tier:** 2 (agent-native) · **Priority rationale:** valuable but narrower in
scope than the fan-out/fan-in/budget features above — provider outages are
real but infrequent relative to the everyday flakiness `attempts:` already
addresses, so this rounds out the tier rather than leading it.

## Feature

Let an agent fall back to a secondary model/provider when the primary
endpoint is unreachable, rather than just retrying a dead connection.

```yaml
agents:
  - name: writer
    source:
      model: big-model
      api_key_env: PRIMARY_KEY
    fallback:
      - source:
          endpoint: https://backup-provider/v1
          model: equivalent-model
          api_key_env: BACKUP_KEY
```

## Additional feature details

- **Quality transparency.** A fallback model may produce meaningfully
  different output than the primary — when a run actually used a fallback
  source, that fact should be visible in the run's recorded output/logs, not
  just silently absorbed. Otherwise a degraded result looks identical to a
  normal one, and nobody investigates a quality dip that was actually a
  provider outage.
- **Scope: connection failures only.** Failover should trigger on
  connection-level errors (timeout, unreachable, 5xx from the provider) —
  not on the model refusing a request or a tool call failing, which are
  different failure classes `attempts:` and normal error handling already
  cover. Conflating the two would mean a legitimate model refusal silently
  retries against a different, possibly less-suitable model.
- **Bounded total attempts.** `attempts:` applying per-source (as the source
  doc states) needs an overall ceiling too — with a primary and two
  fallbacks each retried 3 times, that's up to 9 attempts for one step,
  which could be a long, expensive tail before anyone notices something's
  wrong. Worth capping total attempts across all sources combined, not just
  per source.
- **Hashing.** Only the primary source folds into `AgentContentMap` — which
  source actually served a given run is availability, not content, so a
  fallback happening on one run and not another must not change the step's
  cache key (stated in the source doc, worth re-emphasizing since it's easy
  to get backwards).
- **Same endpoint validation applies to every fallback entry.** The existing
  `validateAgentEndpoints` (no userinfo in `endpoint:`) needs to run against
  each fallback source too, not just the primary — an easy gap to miss since
  it's a repeated structure rather than a single field.
