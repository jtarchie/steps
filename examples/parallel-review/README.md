# parallel-review

True concurrent **fan-out / fan-in** (fork/join): one change is reviewed from
three independent angles _at the same time_, then a lead folds the three
verdicts into a ship decision.

```
        ┌─ security ─┐
review ─┼─ performance ─┼─▶ verdict ─▶ done
        └─ docs ─────┘
   (fork)   (3 hermetic         (join reads
            branch sub-runs)     by label)
```

## What it exercises

| Feature                                 | Where                                                                                                                |
| --------------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| Concurrent fan-out to distinct handlers | `review.parallel: { security, performance, docs }`                                                                   |
| Bounded concurrency                     | `concurrency: 3` (branches run at once; serial under `--mock`)                                                       |
| Hermetic branches                       | each branch is a child run seeded with the pre-fork scope — no branch sees a sibling                                 |
| Barrier join / fan-in                   | `verdict` reads the label-keyed aggregate `({ review }) => review.security.risk …`                                   |
| Failure policy                          | `onBranchFailure: "fail"` — a crashing reviewer fails the review (`"collect"` would continue and report `_failures`) |
| Durable, resumable fork                 | a crash mid-fork reattaches to the same branch children on `steps resume` — no re-run of a finished branch           |

Like `pr-review`, it has **two front doors**: `fetch_pr` (`gh.pr_diff`) passes a
supplied `diff` through untouched (fixtures/CI — hermetic) and only fetches live
from a GitHub `pull_request` webhook (`steps serve --hook workflow.ts` →
`POST /hooks/parallel-review`). When a real `pr` is present, the ship decision
is posted back with `gh.comment`; fixture/offline runs (no `pr`) skip that tail.

## Run it

```sh
steps validate examples/parallel-review/workflow.ts     # accepts + draws the fork
steps run examples/parallel-review/workflow.ts \
  --input diff=@examples/parallel-review/fixtures/change.diff \
  --mock examples/parallel-review/mock_responses.yaml

# Live: fetch the diff and post the ship decision back on the PR
steps run examples/parallel-review/workflow.ts --input pr=123 --input repo=owner/repo
# Or via webhook:
steps serve --hook examples/parallel-review/workflow.ts --hook-token parallel-review=$SECRET
```

Deterministic trace (mock): the parent enters `fetch_pr`, `review` (the fork),
then `verdict` — it never enters a branch state, because the branches run in
their own child runs. `verdict.ship` is `true`. In the web view (`steps serve`)
the branch child runs are listed under the parent's **Branch runs** section, and
the diagram draws the fork's fan-out edges.

## Notes

- The fork is one journal entry in the parent (`fork_started` pins the branch
  child run IDs), so the parent stays single-cursor and fully resumable.
- Branches cannot park (no human gates) or `adopt`/`history` across the fork
  boundary in v1 — a branch gets the pre-fork scope snapshot, not sibling or
  prior conversations.
- Budgets and the wall-clock `limits.timeout` are enforced at the barrier and
  per branch; a hung branch is cancelled at the fork deadline.
