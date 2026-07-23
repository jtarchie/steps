# 000 — `attempts:` and `timeout:` on every step

**Source:** [robustness.md #3](../robustness.md#3-attempts-and-timeout-on-every-step)
**Tier:** 1 (Concourse gap) · **Priority rationale:** smallest surface area, no
new step kind, immediately reduces the most common real-world failure (flaky
`gh` calls, hung resource checks). Ships before anything else needs it.

## Feature

Generalize the agent step's existing `attempts:` field to `get`/`task`/`put`
steps, and add `timeout:` to all four step kinds.

```yaml
- get: repo
  attempts: 3
  timeout: 2m
- task: flaky-integration
  run: ./ci/integration.sh
  attempts: 2
  timeout: 30m
```

## Additional feature details

- **Hook firing.** `on_failure`/`on_error` must fire once, after the final
  attempt is exhausted — not after every failed attempt. A user watching
  build output should see "attempt 1/3 failed, retrying" without the failure
  hooks (e.g. a Slack `put`) firing three times for one logical failure.
- **`fix:` interaction.** A task step can already carry a `fix:` agent that
  reruns the command after repair. Attempts and fix are orthogonal:
  `attempts:` retries the *unmodified* command; `fix:` retries after the
  agent edits something. Precedence needs to be explicit in docs — does
  `fix:` get its own attempt budget, or share the step's? Recommend: `fix:`
  runs once per exhausted attempt, not once total, so a fix that doesn't
  stick still gets `attempts:` tries.
- **Assert interaction.** `assert:` should evaluate only the *final*
  attempt's captured output/exit code, not any intermediate one — an assert
  that happens to match a failed early attempt shouldn't short-circuit
  retries.
- **Timeout classification.** A timed-out step should classify as `errored`
  (distinct from a step that ran to completion and failed), matching the
  existing `outcome.Classify` vocabulary — this determines which hook fires.
- **Not hashed.** Both fields are operational limits, not content — same
  treatment as `assert:`. A pipeline that adds `attempts: 3` to an existing
  step must not invalidate its cache.
- **Visibility.** Log output needs a clear "attempt 2/3" marker per retry so
  a scrollback doesn't read as three unrelated failures.
- **Open question:** should there be a job-level or global default timeout
  (useful under `steps watch` so one hung step can't wedge the poller
  forever), or must every step opt in explicitly? Recommend starting
  explicit-only; a global default is easy to add later without a hash
  change (also operational, not content).
