# 013 — Watch circuit breaker

**Source:** [robustness.md #14](../robustness.md#14-watch-circuit-breaker)
**Tier:** 2 (agent-native) · **Priority rationale:** last in sequence — it's a
protective feature for a job that's *already* misbehaving under long-running
`steps watch` usage, so it matters most once the other Tier 1/2 features
(especially [003](003-passed-cross-job-fan-in.md) and
[004](004-serial-groups.md)) are in place and pipelines are running
unattended for longer stretches.

## Feature

Stop a job from retriggering forever once it's clearly broken, instead of
burning model spend (or CI minutes) on a failure no automatic retry will fix.

```yaml
jobs:
  - name: nightly-summary
    max_consecutive_failures: 3   # then pause; steps jobs resume <name>
```

## Additional feature details

- **Counts runs, not attempts.** `max_consecutive_failures` should count
  distinct *triggered runs* of the job (each a new version), not
  in-run retries already covered by [000](000-attempts-and-timeout.md)'s
  `attempts:` — conflating the two would trip the breaker on ordinary
  flakiness a retry would have absorbed, which is the opposite of the
  intent.
- **Tripping needs to be loud.** A breaker that trips silently defeats its
  own purpose — the whole point is "someone should know this stopped."
  Should tie into the same notification path
  [011](011-approval-steps.md) needs (a hook firing a `put` to
  Slack/email), not just a log line that scrolls by.
- **Resume is manual in v1.** Any successful `steps run` (or a dedicated
  `steps jobs resume <name>`) resets the counter. Auto-resume after a time
  window is a reasonable future addition but should be explicitly deferred
  rather than assumed — an unattended auto-resume defeats the safety
  purpose if the underlying breakage (e.g. a dead API key) hasn't actually
  been fixed.
- **Agent-native hook.** Since tripping can fire the job's `on_error` chain,
  and that chain can itself be an agent step, the interesting version of
  this feature is "the breaker trips, and an agent step in the failure hook
  investigates and files an issue automatically" — worth calling out in
  docs/examples as the feature's actual differentiator over a plain
  circuit-breaker pattern, since the hook mechanism already exists to make
  this free.
- **Per-job, not global.** One misbehaving job pausing should not affect
  unrelated jobs' polling/triggering — worth stating explicitly since
  `steps watch` runs one poller across all jobs in a pipeline.
