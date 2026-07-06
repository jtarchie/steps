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
  context: _"for a larger model, what deserves review here?"_ It gathers leads;
  it never reviews. With `--input root=<checkout>`, `diff.split` attaches the
  current file (capped at `context_bytes`, an action arg) so scouts see the code
  around the patch, not just hunks — deterministic enrichment, the machine
  assembles it.
- **The senior pulls context on demand**: it carries a `file.read` tool with
  three guards — `maxCalls: 3`, a guard allowing **only paths that are part of
  this PR**
  (`({split_diff, args}) => split_diff.files.some(f => f.path ===
  args.path)`),
  and machine-pinned `args` for the repo root so the model never authors it and
  cannot escape it. The model decides _when_ it needs a full file; the machine
  decides _what_ it may touch.
- **`scout_pr`** asks the same question at whole-PR altitude — cross-file
  concerns (docs promising what code retracts, APIs changed without callers) —
  seeing only file stats and scout conclusions, never full patches.
- **`deep_review`** is the senior: it fans out **only over flagged files**
  (`scout_files.items.filter(i => i.risk !== "low")` in the over mapper, which
  also joins each lead with its patch), each with its patch and pre-gathered
  leads. Its job is verify-or-refute, never research.
- **Two guard showpieces**: the scout can propose `trivial`, but the guard only
  allows skipping the senior when _no_ file scout was worried; and the senior
  can propose `approve`, but the guard vetoes it while substantiated findings
  exist. Agent proposes, guards dispose.
- **The trivial path never invokes the large model at all** — a docs-only PR
  costs three small-model calls.
- **Spend controls, all declared in the machine**: `memo: true` on every agent
  state (re-review a PR and only changed files re-pay — a re-run of an unchanged
  PR costs zero tokens); a model-routing function sends medium-risk files to the
  small model; `models:` aliases keep the machine readable;
  `forEach: {concurrency: 3, onItemFailure: "skip"}` parallelizes scouting and
  survives poisoned files.
- **One machine, two front doors.** `fetch_pr` (`gh.pr_diff`) is the seam: a
  supplied `diff` is passed through untouched (fixtures, `--input diff=@…`, CI —
  no `gh` call, fully hermetic); an empty one is fetched live from a `pr`. So
  the same machine runs offline on a fixture and online from a GitHub webhook.
- **Webhook trigger.** The `webhook:` block maps a GitHub `pull_request` event
  to inputs (`steps serve --hook workflow.ts` → `POST /hooks/pr-review`). GitHub
  already sends the title/body and PR number, so only the diff is fetched.
  Drafts and irrelevant actions (closed/edited) are folded into `action` and
  bounced by `skipEvent` **before** any `gh` call or model token is spent.
- **Publish back.** When a real `pr` is present the machine writes to the PR:
  one **inline comment per finding** at its `file:line` (`gh.review_comment`,
  `retry:none`+`skip` so a line GitHub rejects is dropped, not fatal), the
  verdict summary as a top-level comment (`gh.comment`), a commit check
  (`gh.status`, green/red from the `clean` guard), and a triage label
  (`gh.label` — `changes-requested`/`reviewed`). `gh.pr_meta` supplies the head
  SHA the inline comments and check need. Fixture/offline runs (no `pr`) route
  to `done` before this tail and never touch `gh`.

## Run it

```sh
# Deterministic (CI): deep path — findings, vetoed approve
steps run workflow.ts \
  --input diff=@fixtures/pr.diff \
  --input "title=queue: parallel worker pool" \
  --input "description=Process jobs concurrently" \
  --mock mock_responses.yaml

# Deterministic: trivial path — senior never runs
steps run workflow.ts --input diff=@fixtures/pr.diff --mock mock_trivial.yaml

# Live, local: pass a diff you already have (root = the checkout it applies to)…
gh pr diff 123 > pr.diff
steps run workflow.ts --input diff=@pr.diff --input root=.
# …or let the machine fetch it (and post back, if you pass a repo):
steps run workflow.ts --input pr=123 --input repo=owner/repo

# Live, webhook: serve the hook; point a GitHub "Pull requests" webhook at
#   https://<host>/hooks/pr-review?token=$SECRET
# (or set $SECRET as the webhook secret to verify HMAC X-Hub-Signature-256).
steps serve --hook workflow.ts --hook-token pr-review=$SECRET

# Review artifact (always written)
cat out/review.md
```

The gh action pack this exercises — `gh.pr_diff`, `gh.pr_meta`,
`gh.review_comment`, `gh.comment`, `gh.status`, `gh.label` — is documented in
`docs/github.md`.

The fixture diff plants real bugs: a mutex deleted around a now-concurrent map
write, `wg.Add` inside the spawned goroutine, and a swallowed `store.Find` error
— plus a README that documents a `WORKERS` variable the code never reads (a
cross-file lead only the PR-level scout can catch).

## What CI asserts (deep path)

- State sequence
  `fetch_pr, split_diff, scout_files, scout_pr, deep_review, verdict,
  write_review`;
  7 transitions. (`fetch_pr` passes the fixture diff straight through; with no
  `pr` the run ends at `write_review` and never reaches the live publish tail.)
- `scout_files.count == 3` (one hermetic context per file);
  `deep_review.count == 2` (the low-risk README never reached the senior).
- The verdict event was `approve`, but the fired transition was the fallback —
  the guard veto is visible in the journal (`on` is empty).
- `out/review.md` contains the substantiated findings.

Trivial path: state sequence ends `scout_pr, note_trivial`; `deep_review` and
`verdict` never enter; the large model is never resolved.
