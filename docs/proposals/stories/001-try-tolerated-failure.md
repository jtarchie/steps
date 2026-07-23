# 001 — `try:` tolerated failure

**Source:** [robustness.md #4](../robustness.md#4-try-tolerated-failure)
**Tier:** 1 (Concourse gap) · **Priority rationale:** small wrapper, no new
concurrency model, and directly builds on [000](000-attempts-and-timeout.md)'s
retry/outcome plumbing — natural second step before the bigger fan-out work.

## Feature

Wrap a step so its real failure is recorded but doesn't fail the plan —
for best-effort work like notifications or metrics pushes.

```yaml
- try:
    put: slack-notify
    params:
      text: "build finished"
```

## Additional feature details

- **Routing still needs a real outcome.** A `try:`-wrapped step can still be
  the target of `to:` — but it must route on its *actual* success/failure,
  not the swallowed "always succeeds" outcome, or `to:`/`verdicts:` become
  meaningless on tried steps. The plan-level walker sees "success" (never
  blocks downstream); a `to:` on the wrapped step itself sees the truth.
- **Test-mode visibility.** `steps test`'s `assert:` must check the inner
  step's real outcome, not the swallowed one — otherwise a `try:`-wrapped
  step that's silently broken passes every regression run forever, which
  defeats the entire point of `examples/flow.yml`'s self-verifying jobs.
- **Log/output clarity.** Output must clearly flag "step failed (tried,
  continuing)" — a plan that reads all-green in the terminal but quietly ate
  a failure is worse than no `try:` at all if it isn't visible.
- **Composability.** Should nest cleanly with `attempts:`/`timeout:` on the
  inner step (retry a few times, *then* shrug) and with `fix:` on a task
  (attempt repair, then tolerate if the fix doesn't stick).
- **Hooks still fire.** The inner step's own `on_failure`/`on_error` hooks
  should fire normally (e.g. to log the swallowed failure somewhere) even
  though the plan continues — `try:` changes what the *plan* sees, not what
  the *step* experienced.
- **Open question:** does `try:` need a `to:` target of its own for "tried
  and failed" as a third outcome value, or is checking the inner step's
  outcome via existing hooks sufficient? Recommend starting without a new
  outcome key — hooks already cover the "someone should know" need.
