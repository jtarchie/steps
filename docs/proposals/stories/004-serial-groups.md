# 004 — `serial:` / `serial_groups:` job-level mutual exclusion

**Source:** [robustness.md #6](../robustness.md#6-serial--serial_groups--job-level-mutual-exclusion)
**Tier:** 1 (Concourse gap) · **Priority rationale:** directly follows
[003](003-passed-cross-job-fan-in.md) — both are about making multi-job
pipelines under `steps watch` behave correctly, and this closes the remaining
gap where `--max-concurrent > 1` can run the same job twice or let two jobs
race on the same resource.

## Feature

Prevent unsafe concurrent execution of the same job, or of jobs that share a
mutation target (e.g. two jobs that both `put` the same deploy resource).

```yaml
jobs:
  - name: deploy-staging
    serial: true                 # never two builds of me at once
  - name: deploy-prod
    serial_groups: [deploy-lock] # never concurrent with anyone in the group
```

## Additional feature details

- **Queued-vs-locked visibility.** A run that's dequeued from the trigger
  queue but blocked on a lock needs a distinct state from "waiting to be
  dequeued" — otherwise users watching `steps watch` output can't tell
  "nothing has picked this up yet" from "something is actively holding the
  lock."
- **Preemption policy.** If job `deploy-staging` is queued for version A and
  a newer version B arrives before A starts, should B supersede A in the
  queue (Concourse allows this for `serial` jobs), or must every queued
  version run in order? Recommend deciding explicitly rather than leaving it
  implicit — silently dropping A without saying so would surprise users
  expecting every trigger to produce a run.
- **Scope is single-process.** This locks within one running `steps watch`
  process (backed by its own SQLite store), matching every other concurrency
  guarantee in this codebase. It does not protect against two independently
  started `steps watch` processes against the same `.steps/` directory — that
  would need distributed locking and is out of scope; worth stating this
  limitation up front so it isn't assumed to cover that case.
- **Interaction with `passed:`.** A job gated by both `passed:` and
  `serial_groups:` has two independent reasons to not run yet — log output
  should distinguish "waiting on passed:" from "waiting on lock" so
  debugging doesn't require guessing which gate is active.
