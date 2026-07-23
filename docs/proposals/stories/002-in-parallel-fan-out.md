# 002 — `in_parallel` fan-out within a plan

**Source:** [robustness.md #1](../robustness.md#1-in_parallel--fan-out-within-a-plan)
**Tier:** 1 (Concourse gap) · **Priority rationale:** the keystone feature —
`across:` ([005](005-across-matrix-fan-out.md)), `ensemble:`
([008](008-ensemble-agents.md)), and `race:` ([009](009-race-speculative-execution.md))
are all aggregation policies over the same branch runner this introduces.
Everything downstream of it in the sequencing gets cheaper to build once this
exists.

## Feature

Run a set of steps concurrently instead of serially. Today a plan is strictly
sequential — three independent gets, or a lint task and a test task, wait on
each other for no reason, and one slow resource check stalls everything
behind it.

```yaml
jobs:
  - name: verify
    plan:
      - in_parallel:
          limit: 2          # max in flight (default: unbounded)
          fail_fast: true   # first failure cancels the rest
          steps:
            - get: repo
            - get: rules
            - task: lint
              run: golangci-lint run
            - task: test
              run: go test ./...
```

## Additional feature details

- **Naming collisions.** Two branches producing the same output/artifact name
  is a load-time error, same category as `ValidateArtifactFlow`'s existing
  checks — silent last-write-wins would be a correctness trap.
- **Workspace guidance.** Under the default shared workspace, concurrent
  writers stepping on each other's files is a real risk. This feature should
  ship with explicit documentation (and maybe a load-time warning) that
  parallel branches which write should use `workspace: {strategy: copy}` or
  `btrfs` isolation — it doesn't have to *require* it, but users need to be
  told before they hit the race.
- **Log interleaving.** Concurrent steps' stdout/stderr needs a branch label
  prefix in output, or a scrollback with three tasks running at once is
  unreadable. This is a UX requirement, not just a nice-to-have — it's the
  difference between the feature being usable and being avoided.
- **`to:`/`verdicts:` scope.** Routing targets inside a branch stay within
  that branch's own segment (mirrors the existing hook-step restriction) —
  a branch can't route to a step in a sibling branch or outside the
  `in_parallel` block.
- **Nesting.** Should `in_parallel` nest inside itself (parallel groups
  within parallel groups)? Recommend allowing it from day one — the
  alternative is a confusing "flatten your nesting" restriction users will
  hit immediately once they combine this with `across:`.
- **Resource contention.** If several parallel steps each set `image:` and
  spin up containers, host CPU/memory/docker-daemon load multiplies. Not a
  blocker, but worth a documented caution alongside `limit:` — the `limit:`
  field exists partly *for* this reason, not just to bound cost.
- **Fail-fast semantics.** When `fail_fast: true` cancels siblings, in-flight
  agent steps need their context cancellation honored cleanly (no orphaned
  model calls) — worth flagging as a cross-cutting requirement other
  cancellation-based features ([009](009-race-speculative-execution.md)) also
  depend on.
