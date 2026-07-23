# 003 — `passed:` cross-job fan-in on get steps

**Source:** [robustness.md #2](../robustness.md#2-passed--cross-job-fan-in-on-get-steps)
**Tier:** 1 (Concourse gap) · **Priority rationale:** independent of
`in_parallel`, and the single highest-value feature for multi-job pipelines —
without it, `steps watch` can trigger `deploy` on a commit `test` already
failed. Ships right after the fan-out keystone since it's the fan-in
counterpart the doc's own framing calls out.

## Feature

Only run a job's get against versions that already succeeded in other named
jobs — the true "fan-in": two upstream jobs converge on one downstream job
that only proceeds on their shared-green version.

```yaml
jobs:
  - name: unit
    plan:
      - get: repo
        trigger: true
      - task: test
        run: go test ./...

  - name: deploy
    plan:
      - get: repo
        trigger: true
        passed: [unit, lint]   # only versions green in BOTH jobs
      - put: release
```

## Additional feature details

- **No eligible version yet.** On a fresh pipeline (or after a long streak of
  upstream failures), no version may satisfy `passed:` at all. This must be
  a visible, expected "waiting" state under `steps watch` — not silence and
  not an error. Users need a way to see *why* `deploy` hasn't run
  ("no version has passed unit+lint yet").
- **Manual run bypass.** Should `steps run --job deploy` (a single explicit
  invocation, not `watch`'s auto-trigger) honor `passed:`, or bypass it since
  the user explicitly asked for this job right now? Recommend bypassing for
  manual `run` (mirrors Concourse's manual-trigger override) — `passed:`
  gates automatic triggering, not deliberate operator action.
- **Diamond consistency.** When two jobs converge on one downstream job
  (`passed: [unit, lint]`), both upstream jobs must have succeeded against
  the *same* version of the resource, not independently-passing different
  versions — this is what actually makes it a fan-in rather than two
  unrelated gates. Worth stating as an explicit acceptance criterion.
- **Interaction with `version: every`.** If a get step also fans out per
  version (`version: every`), the `passed:` filter must apply *before* that
  fan-out — otherwise the job re-runs once per historical version instead of
  once per newly-eligible one.
- **Observability.** `steps watch` logs (and ideally a `steps status`-style
  command) should be able to answer "what's the latest version that's passed
  each required job" on demand — this is the kind of state users will want
  to inspect when debugging why a deploy hasn't fired.
