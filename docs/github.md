# github — the `gh` action pack

A machine reviews a pull request the way a person does: read the diff, judge it,
say something back. The `gh.*` actions are the read/write surface for that,
shelling out to the GitHub CLI (`gh`), which carries its own auth — no token
plumbing in the machine. They pair with the `webhook:` trigger (see
[webhook.md](./webhook.md)) so the same machine runs from a fixture offline and
from a live `pull_request` event online. `examples/pr-review/` and
`examples/parallel-review/` are the worked examples.

## The actions

| Action              | Reads / writes                         | Key args                                                         | Output                                                                                                                   |
| ------------------- | -------------------------------------- | ---------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `gh.pr_diff`        | a PR's unified diff + title/body       | `pr`, `repo?`, **`diff?`**                                       | `{ diff, title, description }`                                                                                           |
| `gh.pr_meta`        | a PR's routing signals                 | `pr`, `repo?`                                                    | `{ number, title, body, draft, author, isBot, headSha, headRef, baseRef, labels[], additions, deletions, changedFiles }` |
| `gh.comment`        | posts a plain PR comment               | `pr`, `body`, `repo?`                                            | `{ posted, url }`                                                                                                        |
| `gh.review_comment` | posts an inline comment at `file:line` | `repo`, `pr`, `commit_id`, `path`, `line`, `body`, `side?`       | `{ posted, url }`                                                                                                        |
| `gh.status`         | sets a commit status/check             | `repo`, `sha`, `state`, `context`, `description?`, `target_url?` | `{ posted }`                                                                                                             |
| `gh.label`          | adds/removes PR labels                 | `pr`, `add?[]`, `remove?[]`, `repo?`                             | `{ labeled }`                                                                                                            |
| `gh.post_review`    | posts a formal review                  | `pr`, `body`, `event?`, `repo?`                                  | `{ posted, event }`                                                                                                      |

Notes:

- **`gh.pr_diff` is the two-mode seam.** Pass a non-empty `diff` and it is
  echoed straight back (title/description too) **without calling `gh`** — this
  is what keeps fixtures and CI hermetic. Leave `diff` empty and give a `pr` and
  it fetches live. One `fetch_pr` state, two front doors, no branching:

  ```ts
  const fetch_pr: State = {
    action: "gh.pr_diff",
    input: ({ pr, repo, diff }) => ({
      pr: pr || "",
      repo: repo || "",
      diff: diff || "",
    }),
    output: { diff: "string", title: "string", description: "string" },
  };
  ```

- **`gh.comment` vs `gh.post_review`.** A _review_ (`gh.post_review`) cannot be
  left on your own PR and carries an approve/request-changes verdict; a plain
  _comment_ (`gh.comment`) always can. For a bot leaving feedback, reach for
  `gh.comment`.

- **`gh.pr_meta` is for guards, not prose.** Its `draft`/`isBot` fields let a
  machine skip drafts and bot PRs before spending tokens; its `headSha` is the
  `commit_id` an inline `gh.review_comment` needs and the `sha` `gh.status`
  sets.

- **Errors are real errors.** A non-zero `gh` exit (missing auth, unknown PR)
  surfaces as an `action_error` — the engine classifies it transient and retries
  with backoff. This is unlike `exec.run`/`http.get`, whose non-zero/​non-2xx
  results are returned as _data_ for a guard to route on.

- **Args are machine-authored.** As with every action, `input:` is rendered from
  the machine, never model text — safe to shell out with. Do not expose the
  write actions (`gh.comment`, `gh.status`, …) as agent `tools` where a model
  would author the args.

## Guarding the write path

Outbound `gh` calls must not fire on an offline/fixture run. The pattern both
examples use: fetch is a passthrough (no `pr` → no network), and every write is
behind a `pr`-present guard, so a run with no real PR routes to `done` before
it:

```ts
// after the review is written to disk…
branch(write_review, [
  when(({ pr }) => Boolean(pr)).to(publish), // real PR → write back
  done, // fixture/offline → stop
]);
```

`publish` is a
`pipe(fetch_meta, post_inline, post_comment, set_status,
label_pr)` of `gh.*`
action states: `fetch_meta` (`gh.pr_meta`) supplies the head SHA, `post_inline`
is a `forEach` over the findings dropping a `gh.review_comment` at each
`file:line`, then the summary `gh.comment`, the `gh.status` check, and a
`gh.label`. In CI you pass `diff=@fixtures/pr.diff` and **no** `pr`; the whole
tail is skipped and the run never touches `gh`.

The inline `forEach` uses `retry: "none"` + `onItemFailure: "skip"` so a line
GitHub rejects (a 422 for a line outside the diff) drops that one comment
instead of failing the review — the model's line numbers are best-effort.

## Triggering from a GitHub webhook

Declare a `webhook:` block that maps a GitHub `pull_request` event to inputs.
The payload already carries the PR number and the title/body, so only the diff
is fetched (by `fetch_pr`). Fold "draft" into the action so a cheap guard can
bounce non-reviewable events before any `gh` call or model token:

```ts
webhook: {
  path: "pr-review",
  map: ({ body }) => ({
    pr: String(body.number),
    repo: body.repository.full_name,
    title: body.pull_request.title,
    description: body.pull_request.body || "",
    action: (body.pull_request && body.pull_request.draft) ? "draft" : body.action,
  }),
}
```

```sh
steps serve --hook examples/pr-review/workflow.ts --hook-token pr-review=$SECRET
```

**Auth.** GitHub webhooks cannot set an `Authorization` header, so a hook secret
reaches `steps` one of two ways, both off the _same_ configured secret:

- **HMAC signature** — set `$SECRET` as the webhook's _secret_ in GitHub. Each
  delivery carries `X-Hub-Signature-256: sha256=<hmac>`; `steps` verifies it
  over the raw body (constant-time). This is the recommended, canonical model.
- **Query token** — put the secret in the payload URL:
  `https://<host>/hooks/pr-review?token=$SECRET`. GitHub preserves the query
  string, so no header is needed. Handy for a quick setup.

Configure the GitHub webhook to send only **Pull requests**. See
[webhook.md](./webhook.md) for the queue, backpressure, and dispatcher story.

## Local invocation

The same machine runs from `steps run`, no server:

```sh
# pass a diff you already have (hermetic — no gh)
steps run examples/pr-review/workflow.ts --input diff=@pr.diff --input root=.

# or let it fetch and post back (needs gh auth + a repo for the comment/status)
steps run examples/pr-review/workflow.ts --input pr=123 --input repo=owner/repo
```
