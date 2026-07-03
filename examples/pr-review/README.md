# pr-review

Cheap scouts, expensive specialist. The machine encodes a token funnel:

```
split_diff ──▶ scout_files ──▶ scout_pr ──┬─(trivial + guard)──▶ note_trivial ─▶ done
 (action)      (foreach,        (small,   │
                small model,     whole-PR │
                1 file = 1       view)    └─(fallback)─▶ deep_review ─▶ verdict ─▶ write_review ─▶ done
                context)                        (foreach over flagged     (large)     (action)
                                                 files only, large model)
```

- **`scout_files`** asks a small model, per file, in a hermetic per-file
  context: *"for a larger model, what deserves review here?"* It gathers
  leads; it never reviews. With `--input root=<checkout>`, `diff.split`
  attaches the current file (capped at `context_bytes`, an action arg) so scouts see the
  code around the patch, not just hunks — deterministic enrichment, the
  machine assembles it.
- **The senior pulls context on demand**: it carries a `file.read` tool with
  three guards — `maxCalls: 3`, a guard allowing **only paths that are part
  of this PR** (`({split_diff, args}) => split_diff.files.some(f => f.path ===
  args.path)`), and machine-pinned `args` for the repo root so the model
  never authors it and cannot escape it. The model decides *when* it needs a
  full file; the machine decides *what* it may touch.
- **`scout_pr`** asks the same question at whole-PR altitude — cross-file
  concerns (docs promising what code retracts, APIs changed without callers)
  — seeing only file stats and scout conclusions, never full patches.
- **`deep_review`** is the senior: it fans out **only over flagged files**
  (`scout_files.items.filter(i => i.risk !== "low")` in the over mapper, which also joins each lead with its patch), each with its patch
  and pre-gathered leads. Its job is verify-or-refute, never research.
- **Two guard showpieces**: the scout can propose `trivial`, but the guard
  only allows skipping the senior when *no* file scout was worried; and the
  senior can propose `approve`, but the guard vetoes it while substantiated
  findings exist. Agent proposes, guards dispose.
- **The trivial path never invokes the large model at all** — a docs-only PR
  costs three small-model calls.
- **Spend controls, all declared in the machine**: `memo: true` on every
  agent state (re-review a PR and only changed files re-pay — a re-run of an
  unchanged PR costs zero tokens); a model-routing function sends medium-risk
  files to the small model; `models:` aliases keep the machine readable;
  `forEach: {concurrency: 3, onItemFailure: "skip"}` parallelizes scouting
  and survives poisoned files.
- To review real PRs, swap the file plumbing for the gh action pack:
  `action: "gh.pr_diff"` with `input: ({pr}) => ({pr})` up front, and
  `action: "gh.post_review"` with `input: ({verdict, pr}) => ({pr,
  body: verdict.body, event: "comment"})` at the end.

## Run it

```sh
# Deterministic (CI): deep path — findings, vetoed approve
steps run workflow.js \
  --input diff=@fixtures/pr.diff \
  --input "title=queue: parallel worker pool" \
  --input "description=Process jobs concurrently" \
  --mock mock_responses.yaml

# Deterministic: trivial path — senior never runs
steps run workflow.js --input diff=@fixtures/pr.diff --mock mock_trivial.yaml

# Live: scouts on a small local model, senior on a larger one, with full
# file context (root points at the checkout the diff applies to)
gh pr diff 123 > pr.diff   # or use fixtures/pr.diff + fixtures/repo
steps run workflow.js --input diff=@pr.diff --input root=.

# Review artifact
cat out/review.md
```

The fixture diff plants real bugs: a mutex deleted around a now-concurrent
map write, `wg.Add` inside the spawned goroutine, and a swallowed
`store.Find` error — plus a README that documents a `WORKERS` variable the
code never reads (a cross-file lead only the PR-level scout can catch).

## What CI asserts (deep path)

- State sequence `split_diff, scout_files, scout_pr, deep_review, verdict,
  write_review`; 6 transitions.
- `scout_files.count == 3` (one hermetic context per file);
  `deep_review.count == 2` (the low-risk README never reached the senior).
- The verdict event was `approve`, but the fired transition was the fallback
  — the guard veto is visible in the journal (`on` is empty).
- `out/review.md` contains the substantiated findings.

Trivial path: state sequence ends `scout_pr, note_trivial`; `deep_review`
and `verdict` never enter; the large model is never resolved.
