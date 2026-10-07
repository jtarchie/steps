# Resources

A **resource** is something outside the pipeline that has versions: a git branch, a queue of pull requests, a build artifact, a counter in a file. A **resource type** is the code that knows how to talk to one.

Every example on this page (and every other page) is a complete pipeline, verified by the test suite. Blocks that need the network or real credentials say so; everything else runs as shown.

```yaml noexec=network
resources:
- name: repo          # the artifact directory a get creates, and the name steps use
  type: git           # which resource type
  source:             # configuration for that type — free-form, type-specific
    uri: https://github.com/jtarchie/ci.git
    branch: main

jobs:
- name: build
  plan:
  - get: repo         # fetch it — the contents land in ./repo
  - task: compile
    inputs: [repo]
    run: cd repo && go build ./...
```

`git` ships with steps, so that pipeline needs no `resource_types:` block, as do [the GitHub types](#the-built-in-github-types), Slack's and `cron`. For anything else, you write the type yourself.

## The built-in `git` type

| `source:` field | required | meaning |
|---|---|---|
| `uri` | yes | anything `git` can clone — https, ssh, or a local path |
| `branch` | no | omitted follows the remote's `HEAD` |
| `fetch` | no | `true` refreshes a local-path `uri` before each check; default `false` |

A local-path `uri` costs no network clone — the right call for a large repository — but the check reads that clone's refs as they are, so the version is whatever the clone last fetched, however long ago that was. `fetch: true` fixes that: each check first fetches `branch` from the clone's remote (`branch.<name>.remote`, else `origin`) into `refs/remotes/<remote>/<branch>` and reports that ref, so the version is the remote's head. It moves exactly that one ref — never the working tree, a local branch, tags or `FETCH_HEAD` — so a developer's own `main` sitting behind changes nothing. It needs an absolute path and a `branch:` (both checked at load; a remote `uri` is refused, being fresh already), git 2.29 or newer, a clone owned by the user running steps, and auth that never prompts (an ssh agent, a credential helper) — a poll has no terminal, so a prompt fails the fetch instead of waiting. A failed fetch fails the check, so `steps web` reports no version rather than a stale one; like any failing check, it stops that pipeline's poll until the next interval. Turning `fetch:` on changes which commit is reported, and `source:` is part of the cache key, so expect one fresh build. The built-in type refuses any other `source:` key, so a misspelled `fecth:` is a load error rather than a flag nobody reads.

It fetches the exact commit the plan pinned, shallowly, so a branch that moves mid-run still gives you the version that was planned. It has **no `out:`** — `put: repo` against it is a load error, because what "publish" means (which branch, which credentials, force or not) is a decision only you can make. Write your own type for that.

## The built-in `slack-mentions`, `slack-reply` and `slack-reaction` types

Three more built-ins, all [expression-backed](expr.md) — Slack is a JSON HTTP API and nothing else, so there is no container and no `curl`/`jq` dependency to carry. `slack-mentions` is get-only: every unanswered `@mention` of the bot in a channel, plus every message in a 1:1 DM (no `@mention` required there — nobody types one in a 1:1 chat), oldest first, as a `{channel, ts, thread_ts}` version — `ts` is the message that named the bot, `thread_ts` the thread it lives in (the same value, for a top-level message). `slack-reply` is put-only: posts a message, threaded or top-level. `slack-reaction` is put-only too: puts an emoji on a message, takes one off, or swaps one for another in a single publish.

Cold start does not mean a backlog: like every resource, the first-ever check of a freshly-deployed `slack-mentions` records everything it finds and answers only the newest of it — see [Version history](#version-history) below. That rule is a `steps web` behavior; `steps run` has no persisted cursor, so every `steps run` check is a first check.

A cursorless check is also **bounded in what it asks Slack**. Finding a mention written as a reply means one `conversations.replies` call per thread, and the cursor is what normally keeps that set small — no cursor means every thread that has ever been replied to qualifies. On a workspace with more threads than the rate limit allows calls for, that check gets throttled, and a throttled check reports no version, which leaves no cursor for the next one: a watcher that can never run once. So a check with no cursor looks at the five most recently active threads and stops.

Under `steps web` that costs nothing that is ever built: everything below the newest version a first check reports is marked already-taken, and the cursor it leaves behind lifts the bound for every poll after it. Under `steps run` the bound never lifts, because no cursor is ever persisted — a mention written inside the sixth-most-recently-active thread is not delivered. Use `steps web` for a Slack bot; `steps run` is for a one-shot answer to whatever is most recent.

```yaml noexec=network
resources:
- name: mentions
  type: slack-mentions
  source:
    channels: []   # optional; [] (the default) is every channel the bot is in
    limit: 200      # optional; per-channel messages fetched per check, default 200

- name: reply
  type: slack-reply
  source: {}

- name: reaction
  type: slack-reaction
  source: {}

# A second bot, in the same pipeline, answering as someone else: env: adds
# SECOND_BOT_TOKEN to just THIS resource's own allow-list (the type's own
# env: still only names SLACK_BOT_TOKEN), and source.token_env picks it.
- name: reply-as-support-bot
  type: slack-reply
  env: [SECOND_BOT_TOKEN]
  source:
    token_env: SECOND_BOT_TOKEN

jobs:
- name: answer-mention
  plan:
  - get: mentions
    trigger: true
    version: every    # answer every mention found, not just the newest
  - put: acknowledge           # 👀 — somebody is on it
    resource: reaction
    inputs: [mentions]         # the reaction type picks the mention's ts
    params: {add: eyes}
  - task: compose
    outputs: [answer]
    run: echo "got it, working on it" > answer/reply.md
    on_failure:                # ❌, and the 👀 goes away
      put: failed
      resource: reaction
      inputs: [mentions]
      params: {add: x, remove: eyes}
  - put: reply
    inputs: [mentions, answer] # the reply type picks the mention's thread_ts
  - put: answered              # ✅, and the 👀 goes away in the same publish
    resource: reaction
    inputs: [mentions]
    params: {add: white_check_mark, remove: eyes}
```

All three need `SLACK_BOT_TOKEN` (a bot token, `xoxb-`) in the environment, for an app with `chat:write`, `reactions:write`, `channels:history`, `channels:read`, `im:history` and `im:read` (plus `groups:history`/`groups:read` for private channels), installed to the workspace and **invited** to every channel it should watch or post to. Membership is what grants both reading history and appearing in `users.conversations`, so `/invite` *is* the subscribe action — `app_mentions:read` is not needed, since this polls rather than using the Events API.

1:1 DMs are always watched too, with no `@mention` required — there is no `source:` field to turn this off. Group DMs (`mpim`) are not watched at all; a group is closer to a channel (several humans, bot is one more party) than a 1:1, so it keeps the explicit-mention rule instead.

| `source:` field (any of the three) | required | meaning |
|---|---|---|
| `channels` (`slack-mentions`) | no | `[]` watches every channel (and 1:1 DM) the bot is in; a list of ids narrows it |
| `limit` (`slack-mentions`) | no | per-channel messages fetched per check, default `200` |
| `base_url` (any) | no | overrides `https://slack.com` — for pointing a test at a fake server |
| `token_env` (any) | no | overrides `SLACK_BOT_TOKEN` as the env var name to read the token from |

`token_env` alone isn't enough to widen what a resource can read — `env()` only sees names its resource TYPE already declares (all three declare `SLACK_BOT_TOKEN`, shared by every resource of that type), which is what makes it safe for a shared, possibly-external type to hand-in-hand with any expr type at all. A resource naming a different token also needs `env:` *on the resource itself* to add that name to its own allow-list — `env:` and `source:` together, as in `reply-as-support-bot` above. Naming `token_env` without the matching `env:` entry is a run-time error (`env(...): not in this resource type's env:`), not a silent fall-back to `SLACK_BOT_TOKEN`. (`env:` on a resource only means something for an expr- or shell-backed type — an mcp-backed type authenticates via its `mcp_servers:` entry and rejects `env:` at load time.)

**A mention inside a thread arrives with its thread.** `mentions/thread.json` is the whole conversation the mention was written in (Slack's `conversations.replies` payload: parent first, then replies), fetched by `thread_ts` — asking Slack for a *reply's* `ts` answers with that one message and nothing around it, which is an agent being handed a question with no context. `slack-reply` posts the answer back with `thread_ts` too: a reply's `ts` is not a thread id.

**A reaction goes on the message; a reply goes to the thread — and the types choose.** Both read the mention off the put's `slack-mentions` input with [`version()`](expr.md#versionname--version): `slack-reply` posts to its `thread_ts`, `slack-reaction` marks its `ts`, because they want different values out of the same version — `thread_ts` is the conversation to answer in, while `ts` is the one message a person actually wrote. Hand a reaction the thread's id and the emoji lands on the parent of the thread instead — indistinguishable from correct when the mention was top-level, wrong every time somebody asked inside an existing conversation. That choice is made once, in each type, rather than in every pipeline. With more than one fetched input (`inputs: all`, or a second get), `params: {from: mentions}` names the one to answer; without it the put fails naming them.

**Marking a message twice is not an error.** `already_reacted` (the emoji this put wanted is already there) and `no_reaction` (the one it wanted gone is already gone) are the API saying the world is in the state being asked for, so the put succeeds. They arrive on any replay, resume, or re-run, and failing there would turn re-running a build into a red one over an emoji. Every other refusal fails the put — most usefully `missing_scope`, since `reactions:write` is not implied by `chat:write` and a bot that quietly stops marking anything is a failure nobody notices.

**A mention inside a thread counts**, and `limit:` is what decides whether it can be found. Slack's `conversations.history` returns top-level messages only, and a reply does not change its parent's `ts` — so the window the check reads channel history over is `limit` messages, deliberately *wider* than the cursor, and the cursor decides only what inside that window is new. A thread whose parent carries a `latest_reply` newer than the cursor is read; every other thread costs nothing.

**Two known gaps** remain, both bounded by `limit:` and both needing Slack pagination to close:

- A thread whose parent has scrolled past `limit` messages of its channel's history is invisible, however active the thread still is. How long that takes is how chatty the channel is; raising `limit:` buys more room, but only as much as Slack allows: since 2025-05-29 a bot **distributed** outside the Marketplace is capped at 15 messages per request and one request a minute, whatever `limit:` says. An internal app built for a single workspace — the usual case for a self-hosted pipeline — is excluded from that cap and honors `limit:` as written.
- `mentions/thread.json` is truncated at its *newest* end for a thread longer than 1000 messages — Slack returns a thread oldest-first — so the mention itself can be missing from a very long thread.
- More than `limit` new top-level messages in one channel between two checks lose the overflow *permanently*, not just delayed — the cursor advances to the newest `ts` seen anywhere, so whatever `limit` cut off now sits below the new cursor and is never asked for again.

Where each put type aims comes from a fetched `slack-mentions` input (or the one `params.from` names). With no fetched input — a top-level post to a fixed channel, or a message that did not come from a mention — it reads files an upstream step wrote instead. Job-level hooks take no `inputs:` and run outside any one build, so a ❌ belongs on a step's `on_failure:`, as above. The text is always a file: `file()` takes what `inputs:` put on disk directly, so a reply containing backticks or `$(…)` is data, never something a shell might run.

| file | read by | required | meaning |
|---|---|---|---|
| `answer/reply.md` | `slack-reply` | yes | the message text |
| `thread/channel` | `slack-reply` | without a fetched input | the channel id to post to |
| `thread/ts` | `slack-reply` | no | a parent message's `ts` — posts as a reply in that thread; omit to post a new top-level message |
| `target/channel` | `slack-reaction` | without a fetched input | the channel id of the message to mark |
| `target/ts` | `slack-reaction` | without a fetched input | the `ts` of the message to mark |

There is deliberately no `check:`/`in:` on `slack-reply` and no `out:` on `slack-mentions` — `get: reply` or `put: mentions` are both load errors, the same rule `git`'s missing `out:` follows.

## The built-in GitHub types

Five built-ins, split by what they do, the way the Slack ones are. `github-prs` and `github-comments` **find work**: they have a check and an in, and no out. `github-pr-comment`, `github-pr-review` and `github-reaction` **publish**: they have an out and nothing else. The split is not tidiness. A put records its version in its own resource's history, so one type that both watched pull requests and commented on them would put every comment it posted into the history it triggers from.

steps talks to GitHub itself. Nothing needs `gh`, `git` or `curl`, on this machine or any other, and the tree a get fetches is an ordinary artifact here: a task under [`image:`](infra.md#container-execution-image) or [`tags:`](infra.md#remote-workers-tags) reads it the way it reads any other. That also means none of the five can be placed with `tags:` themselves, as for every type that runs inside this process.

All five read a token from `GH_TOKEN`, the variable `gh` reads first, so `export GH_TOKEN=$(gh auth token)` is the whole setup on a machine where `gh` is logged in. `source.token_env` names a different variable, for a second identity. The token is required, not optional: GitHub gives an unauthenticated caller sixty requests an hour, which a poller spends in minutes. `steps validate` refuses a pipeline whose token variable is unset, before anything runs.

### `github-prs`: open pull requests

```yaml github=review
resources:
- name: pr
  type: github-prs
  source:
    repo: acme/app
    review_requested: "@me"      # quoted: @ cannot start a plain YAML value
    base: main
    labels: [ready-for-review]
    draft: false

- name: summary
  type: github-pr-comment
  source:
    repo: acme/app

jobs:
- name: review
  plan:
  - get: pr
    trigger: true
    version: every               # one build per pull request, and per push to it
  - task: read
    inputs: [pr]
    outputs: [notes]
    run: |
      echo "#$(cat pr/pr.number) changes $(grep -c '^diff --git' pr/pr.diff) file(s) since $(cut -c1-7 pr/pr.mergebase)" | tee notes/body.md
      test -x pr/bin/test.sh
    assert:
      stdout: "#42 changes 2 file(s) since 1a2b3c4"
  - put: summary
    inputs: [pr, notes]
    params:
      body_file: notes/body.md
      from: pr                   # optional here: pr is the only input naming a pull request
  assert:
    execution: [pr, read, summary]
    outcome: succeeded
```

A version is `{number, sha}`: the pull request and its head commit, so a push is a new build and a wording change to the title is not. The check reports them by number, oldest first. Every filter narrows, and a pull request is a version only while it is open and matches all of them:

| `source:` field | required | meaning |
|---|---|---|
| `repo` | yes | `owner/name` |
| `author` | no | who opened it |
| `assignee` | no | someone it is assigned to |
| `review_requested` | no | someone whose review it directly requests; GitHub clears the request once they submit one, so the trigger empties itself |
| `labels` | no | labels that must all be on it |
| `base` | no | the branch it would merge into |
| `draft` | no | `true` keeps only drafts, `false` only ready ones; both when unset |
| `checkout` | no | `false` skips the tree and writes only the metadata files; default `true` |
| `token_env` | no | the variable holding the token; default `GH_TOKEN` |
| `endpoint` | no | the REST API base; default `https://api.github.com`, and `https://<host>/api/v3` for GitHub Enterprise Server |

`@me` in any of the three login fields is the token's own user. A get writes the tree at the version's commit, then these beside it:

| file | what it holds |
|---|---|
| `pr.json` | the pull request, as GitHub's API describes it |
| `pr.diff` | the change as of the version's commit, against where it branched from `base` |
| `pr.number`, `pr.sha` | the version |
| `pr.mergebase` | the commit it branched from, for a step that wants to stage the change onto it |
| `pr.url` | its page |

The diff and the merge base are computed against the version's commit rather than read from the pull request, whose own diff is always of its *current* head, so a pinned or replayed version sees the change as it was. A repository that has a file of one of those names at its root is refused by name rather than overwritten; set `checkout: false` and fetch the tree with a `git` resource there.

**Being assigned is a trigger.** An assignment brings a pull request into the filtered set, which makes it a version the resource has never seen:

```yaml github=assigned
resources:
- name: assigned
  type: github-prs
  source:
    repo: acme/app
    assignee: "@me"
    author: alice
    checkout: false                    # metadata only
    token_env: GH_TOKEN                # the default, shown; name another variable to act as someone else
    endpoint: https://api.github.com   # the default

jobs:
- name: pick-up
  plan:
  - get: assigned
    trigger: true
    version: every
  - task: announce
    inputs: [assigned]
    run: echo "picking up $(cat assigned/pr.url)"
    assert:
      stdout: "picking up https://github.example/acme/app/pull/42"
  assert:
    execution: [assigned, announce]
    outcome: succeeded
```

The check is one GraphQL search per poll, because the filters a pipeline asks about are search qualifiers and nothing else: GitHub's pull request listing filters on none of them.

### `github-comments`: a comment as a trigger

```yaml github=comment-command
resources:
- name: command
  type: github-comments
  source:
    repo: acme/app
    author: alice
    body: '^/steps\s+review\b'
    kinds: [conversation, review]    # the default; add issue for comments on issues
    checkout: true                   # the default: the pull request's tree comes with it
    token_env: GH_TOKEN              # the default
    endpoint: https://api.github.com # the default

- name: answer
  type: github-pr-review
  source:
    repo: acme/app

jobs:
- name: act
  plan:
  - get: command
    trigger: true
    version: every
  - task: respond
    inputs: [command]
    outputs: [reply]
    run: |
      echo "asked by $(cat command/comment.author) on #$(cat command/pr.number): $(cat command/comment.body)" | tee reply/body.md
      test -f command/README.md
    assert:
      stdout: "asked by alice on #42: /steps review please"
  - put: answer
    inputs: [command, reply]
    params:
      body_file: reply/body.md
      from: command                  # the input whose version names the pull request
      event: pending                 # a draft only you can see, until you submit it
  assert:
    execution: [command, respond, answer]
    outcome: succeeded
```

| `source:` field | required | meaning |
|---|---|---|
| `repo` | yes | `owner/name` |
| `author` | no | who wrote it, compared without regard to case as GitHub compares logins; `@me` is the token's own user |
| `body` | no | an [RE2](https://github.com/google/re2/wiki/Syntax) pattern the text must contain a match for; `^` anchors it to the start |
| `kinds` | no | `conversation` (a pull request's timeline), `review` (inline, on a line of its diff), `issue` (an issue's timeline); default the first two |
| `checkout` | no | `false` skips the pull request's tree; default `true`. An issue has no tree |
| `skip_reacted` | no | a reaction (`+1`, `-1`, `laugh`, `confused`, `heart`, `hooray`, `rocket`, `eyes`) that, left by the token's own user, marks a comment done: it is not reported |
| `token_env`, `endpoint` | no | as for `github-prs` |

A get writes `comment.json`, `comment.body`, `comment.id`, `comment.kind`, `comment.author` and `comment.url`. A comment on a pull request brings the pull request with it, every `pr.*` file above plus the tree at its current head, because a get cannot be told which pull request to fetch by another get's output. A comment on an issue writes `issue.json` and `issue.number` instead.

**The version is `{id, kind, number, updated}`**, and `updated` is there on purpose, against the usual rule that a version carries identity only. The text is the payload here, so a comment edited from one instruction to another is new work, and an edit that stops matching `body` is not. It is also the [cursor](#the-check-cursor): each check asks GitHub only for comments written since the newest version it last reported, newest first, one page when there is no cursor yet. Every page is a conditional request, and GitHub answers an unchanged listing without charging the rate limit, so a quiet repository costs nothing to poll. Review summaries, the text a reviewer writes when submitting, are not watched: GitHub has no listing of them across a repository.

A pipeline that comments as the same user it watches must make sure what it posts cannot match `body`, or its own reply is the next build.

**`skip_reacted` makes a mark on GitHub the record of what is done.** The cursor keeps a running daemon from reporting a comment twice, but the cursor lives in the state database, and a check with no cursor reads a page of recent comments and builds the newest of them. That is a command already answered — a deploy, a model run, a reply — run again because a file was deleted. A reaction the job leaves on its comment outlasts any database, so a comment carrying `skip_reacted` from the token's own user is not reported at all. Put it on with a [`github-reaction`](#github-reaction-marking-a-comment) put as the job's last step. Only the token's own user counts, because anybody can react with anything.

It stays cheap. A comment is looked at only after it has passed `author` and `body`, and only when GitHub's own summary on it counts at least one of that reaction; only then are its reactions of that kind listed, to see whose they are, as a conditional request like every other page. A lookup that fails fails the check, since reporting a done comment as new is exactly what the field is there to prevent. While every comment on a cursorless first page carries the mark, the check reports nothing and so keeps no cursor, and each poll reads that page again until a new command arrives.

**A reaction does not change a comment's `updated`.** Adding the mark makes no new version, so marking a comment never triggers the job again. Removing it does not bring the comment back by itself either: with a cursor, the check asks only for comments written since the newest version it reported, and an older comment is not among them. It is work again when it is edited, which is a new version anyway, or when a check with no cursor finds it on its first page.

### `github-pr-comment` and `github-pr-review`: publishing

Both take `repo`, `token_env` and `endpoint` in `source:` and nothing else, and find which pull request a put is about in its inputs: the one input whose version names a pull request, which every `github-prs` and `github-comments` get's does. `params.from` picks one when several do, and `params.number` names one outright for a put with none:

```yaml github=post-fixed
resources:
- name: tracking
  type: github-pr-comment
  source:
    repo: acme/app
    token_env: GH_TOKEN                # the default; name another variable to post as someone else
    endpoint: https://api.github.com   # the default
- name: verdict
  type: github-pr-review
  source:
    repo: acme/app

jobs:
- name: report
  plan:
  - task: summarize
    outputs: [report]
    run: |
      echo "all green" > report/body.md
      echo '[{"path": "app/a.rb", "line": 12, "body": "nice guard"}]' > report/comments.json
  - put: tracking
    inputs: [report]
    params:
      body_file: report/body.md
      number: "42"
  - put: verdict
    inputs: [report]
    params:
      body_file: report/body.md
      number: "42"
      event: approve
      comments_file: report/comments.json   # each comment on its line of the diff
  assert:
    execution: [summarize, tracking, verdict]
    outcome: succeeded
```

| `params:` field | put | meaning |
|---|---|---|
| `body_file` | both | the text to post, a path inside the put's inputs; an empty file is refused, since posting nothing is how a step that wrote nothing would otherwise look |
| `from` | both | the input whose version names the pull request, when more than one does; with `in_thread`, the `github-comments` get whose comment it answers |
| `number` | both | the pull request, when no input names it; not with `in_thread` |
| `in_thread` | `github-pr-comment` | `true` answers the comment a `github-comments` get fetched where it was written: an inline comment in its thread, any other kind in the conversation |
| `event` | `github-pr-review` | `comment` (the default), `approve`, `request_changes`, or `pending` |
| `comments_file` | `github-pr-review` | a JSON array of comments on lines of the diff, posted in the same review: `[{"path": "app/a.rb", "line": 12, "body": "..."}]`, with `start_line` for a range and `side: LEFT` for a removed line; an empty array posts the body alone |

`github-pr-comment` posts to the pull request's conversation, or with `in_thread` to an inline comment's thread, as below. `github-pr-review` posts a review on the commit its input fetched when the input is a `github-prs` get, so a push after the fetch does not move the review onto code nobody read. **`event: pending` drafts a review only the token's user can see** until they submit it, and replaces that user's own earlier draft, since GitHub allows one per user per pull request, so a new push's draft takes the old one's place. Nobody else's draft is touched. GitHub refuses an approval or a change request from a pull request's own author; post a comment there instead.

**`comments_file` puts each comment on its line**, so the author reads it beside the code instead of looking a `path:line` up. `line` is the line in the file, not a position in the diff. The file is read and checked before anything is sent, unknown fields included, so a `file:` written for `path:` fails the put by name, and a pending put never deletes the last draft only to fail on the next. Whether a line is in the diff is GitHub's call, since only it knows which lines a review may sit on, and it refuses the whole review if one is not: check the lines against `pr.diff` in a task before the put when a model chose them.

**`in_thread: true` answers a comment where it was asked.** A command written on a line of the diff gets its answer in that line's thread, beside the code, rather than in a conversation the asker has to go and find:

```yaml github=thread
resources:
- name: question
  type: github-comments
  source:
    repo: acme/app
    body: '^/steps\s+explain\b'
    checkout: false
- name: answer
  type: github-pr-comment
  source:
    repo: acme/app

jobs:
- name: explain
  plan:
  - get: question
    trigger: true
    version: every
  - task: write
    inputs: [question]
    outputs: [reply]
    run: |
      echo "looking into $(cat question/comment.kind) comment $(cat question/comment.id)" | tee reply/body.md
    assert:
      stdout: "looking into review comment 702"
  - put: answer
    inputs: [question, reply]
    params:
      body_file: reply/body.md
      in_thread: true                # an inline comment's thread; the conversation for any other kind
      from: question                 # optional here: question is the only input naming a comment
  assert:
    execution: [question, write, answer]
    outcome: succeeded
```

The comment comes from a `github-comments` input and nothing else, as for [`github-reaction`](#github-reaction-marking-a-comment): a put whose inputs hold no such get is refused at load, as is a `from` naming one that is not, and so is `number`, which names a pull request with no comment in it to answer. **Only an inline comment has a thread.** A pull request's conversation and an issue's are flat, so a comment there is answered in the conversation, exactly as without the flag. That is deliberate: a pipeline watching both kinds sets `in_thread` once and gets the right place for each. GitHub's reply route takes only a thread's first comment, while a command is as often written as a reply further down, so the put reads the comment first and answers under the comment that started its thread; the answer lands at the bottom of the same thread either way. The put's version is `{id, number}` with or without the flag: the comment it posted, and the pull request or issue it is on.

### `github-reaction`: marking a comment

A reaction is how a bot says "seen" and "done" on the comment that asked, without a reply in the conversation for every command. `github-reaction` puts one on, takes the token's own one off, or both in one put, on the comment a `github-comments` get fetched:

```yaml github=reaction
resources:
- name: command
  type: github-comments
  source:
    repo: acme/app
    author: alice
    body: '^/steps\s+deploy\b'
    checkout: false
    skip_reacted: rocket             # the done mark, when the token's own user left it
- name: mark
  type: github-reaction
  source:
    repo: acme/app
    token_env: GH_TOKEN              # the default; whose reactions these are
    endpoint: https://api.github.com # the default

jobs:
- name: deploy
  plan:
  - get: command
    trigger: true
    version: every
  - put: seen
    resource: mark
    inputs: [command]
    params:
      add: eyes                      # picked up, before the work starts
  - task: work
    inputs: [command]
    run: |
      echo "deploying for $(cat command/comment.author): $(cat command/comment.body)"
    assert:
      stdout: "deploying for alice: /steps deploy prod"
  - put: done
    resource: mark
    inputs: [command]
    params:
      add: rocket                    # done, and the mark skip_reacted reads
      remove: eyes                   # the token's own eyes; anybody else's stay
      from: command                  # optional here: command is the only input naming a comment
  assert:
    execution: [command, seen, work, done]
    outcome: succeeded
```

| `params:` field | meaning |
|---|---|
| `add` | a reaction to put on: `+1`, `-1`, `laugh`, `confused`, `heart`, `hooray`, `rocket` or `eyes`. Quote `"+1"` and `"-1"`: unquoted, YAML reads them as numbers |
| `remove` | a reaction of the token's own user to take off, from the same set |
| `from` | the `github-comments` get whose comment this marks, when more than one input names one |

At least one of `add` and `remove` is required, and the set is GitHub's, not Slack's: `white_check_mark` is a load error naming the eight there are. **The comment comes from a `github-comments` input and nothing else**, so a put whose inputs hold no such get is refused at load, as is a `from` naming one that is not. The version carries the comment's kind as well as its id, and the kind picks the route: an inline review comment's reactions live under the pull request's comments, every other kind's under the issue's, and the same id on the wrong route is somebody else's comment or none.

**`add` goes before `remove`**, so a swap from `eyes` to `rocket` that fails halfway leaves both marks rather than neither, and a comment with no mark reads as one nobody picked up. **`remove` touches only the token user's own reaction**: the put lists that reaction on the comment, finds the one the token's user left, and deletes that one by id, so somebody else's `eyes` stays where they put it. **Marking twice is not an error.** GitHub answers an add that is already there with the reaction that is, and a remove finds nothing when it is already gone; both are the comment in the state asked for, which a replay or a resume always finds, the way `slack-reaction` reads `already_reacted`. Any other answer fails the put with GitHub's status, its words and the route.

A put's version is `{comment, add, remove}`, recorded in the reaction resource's own history and never in the comments one, which is why a mark cannot trigger anything.

## The built-in `cron` type

`type: cron` mints a version when a slot of a crontab expression passes — the trigger for a nightly report, an hourly sweep, a job that should run whether or not anything else changed. Nothing is fetched: the artifact is the moment itself. A port of [govuk-pay/cron-resource](https://github.com/govuk-pay/cron-resource), kept to its rules.

```yaml
resources:
- name: tick
  type: cron
  source:
    expression: "*/15 * * * *"     # every quarter hour
    location: America/New_York     # optional; the zone the expression is read in, UTC otherwise
    fire_immediately: true         # optional; the first-ever check mints a version instead of waiting

jobs:
- name: sweep
  plan:
  - get: tick
    trigger: true
  - task: show
    inputs: [tick]
    run: test -s tick/epoch && test -s tick/timestamp && grep -c '"time"' tick/version.json
    assert:
      stdout: "1"          # one line holds the version; the two files before it are non-empty
  assert:
    execution: [tick, show]
    outcome: succeeded
```

| `source:` field | required | meaning |
|---|---|---|
| `expression` | yes | a crontab: five fields (`minute hour day-of-month month day-of-week`), six with seconds in front (`*/30 * * * * *`), or a descriptor (`@hourly`, `@daily`, `@weekly`) |
| `location` | no | the zone the expression is read in, an IANA name such as `Europe/London`; UTC when unset |
| `fire_immediately` | no | the first-ever check mints a version at once, instead of only when a slot fell in the last hour |

**A poll is what wakes it.** There is no timer inside the resource: `steps web` reads the clock on every `--interval` (30s unless said otherwise), and the first poll after a slot passes mints the version. So a slot fires within one poll of its time, and a seconds field is honored in the expression but cannot fire faster than the poll — `*/5 * * * * *` under a 30s interval is a version every 30s. One version per poll at most: several slots between two polls fire once, and a daemon that was stopped over the nightly slot does not run it when it comes back.

**The version is `{time}`**, the moment the check ran (RFC3339, UTC) rather than the slot it answered — `02:00:17Z` for a `0 2 * * *` polled at seventeen seconds past. That is what `epoch` means: when the job was released. A get writes three files:

| file | what it holds |
|---|---|
| `version.json` | the version, `{"time": "2026-09-25T02:00:17Z"}` |
| `timestamp` | the same moment in the resource's `location`, RFC3339 — `2026-09-24T22:00:17-04:00` |
| `epoch` | seconds since the Unix epoch, an integer |

**A first check is careful.** With nothing recorded there is no "since", so the check fires only if a slot fell in the **last hour** — a nightly pipeline set at three in the afternoon waits for two in the morning, and an every-ten-minutes one starts within its first poll. `fire_immediately: true` fires at once instead. This is a `steps web` behavior: `steps run` and `steps test` have no cursor, so every run of theirs is a first check — a get of a resource with no slot in the last hour fails as `no versions available`, which is the truth, and `fire_immediately` is how a one-shot run asks for the time regardless.

```yaml noexec=schedule
resources:
- name: nightly
  type: cron
  source:
    expression: "0 2 * * 1-5"      # two in the morning, Monday to Friday
    location: America/New_York

jobs:
- name: report
  plan:
  - get: nightly
    trigger: true
  - task: summarize
    inputs: [nightly]
    run: echo "report for $(cat nightly/timestamp)"
```

There is deliberately no `out:` — `put: tick` is a load error, as it is for `git`. A step that wants the current time has `date`; a version whose only meaning is "now" is not something to publish. `tags:` is refused too, as for every type that runs inside this process: there is nothing to place.

## Writing a resource type

A resource type is three shell commands. (For a resource that is a JSON HTTP API and nothing else, there is a second way to write them — see [expression resource types](expr.md), which trades containers and binary artifacts for concurrent HTTP and no dependency on `curl`/`jq`.) Each is a [template](templating.md) and each runs `sh -c`. This one is self-contained, so it runs anywhere:

```yaml
resource_types:
- name: greetings
  # image: alpine:3   # optional — run these in a container instead of on the host
  config:
    check: |
      printf '[{"word": "hello"}, {"word": "hola"}]'
    in: |
      echo {{ .version.word | shellquote }} > word.txt

resources:
- name: greeting
  type: greetings
  source: {}

jobs:
- name: speak
  plan:
  - get: greeting
  - task: shout
    inputs: [greeting]
    run: tr a-z A-Z < greeting/word.txt
    assert:
      stdout: HOLA           # check printed oldest-first, so the LATEST is "hola"
  assert:
    execution: [greeting, shout]
    outcome: succeeded
```

### `check` — what versions exist?

Runs when a plan is built, and on every `steps web` poll.

- **Sees**: `{{ .source }}` and `{{ .version }}` — the last version this pipeline recorded for the resource. See [the cursor](#the-check-cursor) below.
- **Must print**: a JSON **array** of version objects to stdout, **oldest first**. A version object is a flat map of strings — `{"ref": "abc123"}`, `{"number": "87"}`. The whole object identifies the version; steps never interprets the fields.
- **Empty array** means "no versions yet". Any version mode other than `every` fails the step with `no versions available`. Under `version: every` the job exits 0 having built nothing, and says which kind of nothing it was:
  - `get: <name> cannot build; no versions exist for: <names>` — an input has never had a version at all, so no set can be assembled. The named resources are what to go look at.
  - `get: <name> has no new versions; all N already taken` — the steady state: everything this check reports has been built already. Under `--force` the line says the force did not re-open them, and the run page shows the get as skipped with the same words.
  - `get: <name> returned no versions; the N step(s) after it did not run` — the check came back empty and there is no history to fall back on, so that much plan was dropped.
  - `get: <name> returned no versions; nothing was fetched` — the same, for a get *inside* a build: the rest of the plan still runs, without that artifact.

  These say a check came back empty — they cannot say *why*, so a type should still **fail loudly** (exit non-zero) when it can't answer, rather than printing nothing.
- **Exit non-zero** to fail the step.

```json
[{"ref": "9fceb02"}, {"ref": "d7b22a6"}]
```

#### The check cursor

`{{ .version }}` is the newest version the last successful check reported — Concourse calls it the *current version*. It exists so a check can ask its API for what it has **not seen** instead of guessing a window:

```yaml fragment
# a guess, and the only thing between you and both failure modes below
check: |
  curl -sS ... --data-urlencode 'limit=20' https://api.example.com/messages

# ask for exactly what we haven't seen
check: |
  curl -sS ... --data-urlencode 'since={{ index .version "ts" | default "0" }}' \
               --data-urlencode 'limit=200' https://api.example.com/messages
```

Guess too small and items scroll past during a busy period — and while [history](#version-history) means a version steps already recorded is not lost, one it never saw at all cannot be recovered by anything. Guess anything at all and a cold start sees a backlog it must not answer — it builds only the newest of what it finds.

Three things to know:

- **Spell it `{{ index .version "ts" | default "0" }}`, not `{{ .version.ts }}`.** On the first-ever check there is no cursor and `.version` is an empty map; templates render with `missingkey=error`, so the bare form fails that first poll. This is the same shape an optional `source:` field or get `params:` already uses.
- **The cursor belongs to `steps web` alone**, which both advances it and is the only thing that reads it. A `steps run` or `steps plan` renders `{{ .version }}` as an empty map even when a watcher has been polling for weeks — a manual run asks what exists, not what is new.
- **A check that ignores `.version` keeps working exactly as before.** The cursor narrows what a check *asks for*; it is not a filter steps applies to the answer, and it is not what decides which versions a job builds — that is [history](#version-history).

### Version history

steps remembers every version it has seen of a resource, in the order it first
saw them. That record is what a triggered job actually builds from — it does
**not** re-run `check` for the versions it was triggered for.

The reason is that a cursor-driven check cannot be asked twice. The second
answer is different, because the first answer moved the cursor: a job
re-deriving its own versions would ask "what is new since the versions I was
just handed" and correctly get nothing. A lookup is repeatable, so plan time
and run time agree without anything being passed between them.

History is also what makes a version *recoverable*. Before it, whatever
`check` returned right now was the whole universe — a version that scrolled
out of the window while nothing was watching was gone, and no amount of
cursor bookkeeping could bring it back.

Three things follow:

- **A resource nothing has polled has no history**, so a get against it runs
  its own `check` — every `steps run`, and every `get:` beside a triggered one.
- **A cold start does not become a backlog.** The first check of a resource
  records everything it reports, marks everything BELOW the newest as already
  taken, and triggers once on the newest — so a watcher pointed at twenty
  existing items builds the twentieth and answers none of the nineteen. One
  build, not twenty (which is what the marking is for) and not zero (which
  left a slowly-changing resource unbuilt indefinitely, and made deleting the
  state database re-arm that silence). `version: every` does not change this:
  the fan-out is over UNCONSUMED versions, and a cold start leaves exactly one
  unconsumed. (A job *added* to the pipeline later has no such marking, and
  its first trigger will see whatever history holds.)
- **Pruning is not free.** A version dropped from history takes its green
  record with it, so a `passed:` gate can no longer clear for it. That is
  correct — a version out of history cannot be built — but it means a limit
  set below what a slow downstream job needs will hold that job back.

#### How much to remember

`defaults.version_history:` caps it per resource, keeping the newest (`0` keeps everything, as [every limit here does](attempts-timeout.md#zero-means-no-limit)):

```yaml
# The cap itself is not observable in one run — internal/store/sqlite's tests measure
# the pruning. This example pins only that the field loads and a capped
# resource still fetches normally.
defaults:
  # A git branch produces a version per push; a chat feed one per message.
  # The right number is a property of what you watch.
  version_history: 50

resource_types:
- name: counter
  config:
    check: |
      printf '[{"n": "1"}, {"n": "2"}]'
    in: echo {{ .version.n | shellquote }} > n.txt

resources:
- name: ticks
  type: counter
  source: {}

jobs:
- name: build
  plan:
  - get: ticks
  - task: show
    inputs: [ticks]
    run: cat ticks/n.txt
    assert:
      stdout: "2"
  assert:
    execution: [ticks, show]
    outcome: succeeded
```

`--version-history` sets a default for a pipeline that does not; when neither
says, steps keeps 1000. Whatever the limit, the newest versions are the ones
kept.

```yaml
resource_types:
- name: since-cursor
  config:
    # A real type would send this to an API. Here it just reports what it was
    # handed, which is the part worth seeing: on a fresh run there is no
    # cursor, so the default is what the check gets.
    check: |
      printf '[{"seen": "%s"}]' '{{ index .version "ts" | default "0" }}'
    in: echo {{ .version.seen | shellquote }} > seen.txt

resources:
- name: feed
  type: since-cursor
  source: {}

jobs:
- name: poll
  plan:
  - get: feed
  - task: show
    inputs: [feed]
    run: cat feed/seen.txt
    assert:
      stdout: "0"          # nothing recorded yet, so the check saw the default
  assert:
    execution: [feed, show]
    outcome: succeeded
```

### `in` — fetch one version

Runs when a `get` step executes.

- **Sees**: `{{ .source }}`, `{{ .version }}` (one object from `check`'s array), and `{{ .params }}` (the get step's `params:`).
- **Working directory** is the artifact directory itself, already created and empty. Write the contents there — `.`, not a subdirectory named after the resource.
- **Exit non-zero** to fail the step.

`params:` on a get is how a resource is told *how* to fetch, as opposed to `source:`, which says *what* to fetch. The distinction matters because `source:` belongs to the resource and `params:` belongs to the step, so one resource can be fetched differently by different jobs without being declared twice:

```yaml
resource_types:
- name: notes
  config:
    check: |
      printf '[{"ref": "v1"}]'
    in: |
      head -n {{ index .params "lines" | default "100" }} <<'EOF' > notes.txt
      first line
      second line
      EOF

resources:
- name: log
  type: notes
  source: {}

jobs:
- name: quick
  plan:
  - get: log
    params: { lines: 1 }     # this job fetches a truncated view
  - task: show
    inputs: [log]
    run: wc -l < log/notes.txt
    assert:
      stdout: "1"            # the param reached in:
  assert:
    execution: [log, show]
    outcome: succeeded
- name: full
  plan:
  - get: log                 # same resource, whole thing
  - task: show
    inputs: [log]
    run: wc -l < log/notes.txt
    assert:
      stdout: "2"            # no param, so the default won — same resource, other view
  assert:
    execution: [log, show]
    outcome: succeeded

assert:
  execution: [quick, full]
```

**Optional params take the same shape as an optional `source:` field** (see [Shell safety](#shell-safety) below). Templates render with `missingkey=error`, so a bare `{{ .params.lines }}` makes `lines` *mandatory* on every get of that type; `{{ index .params "lines" | default "100" }}` works on an absent key and on a get with no `params:` block at all.

**Params change the fetch, so they change the hash.** Two gets of one version differing in `params:` are two different fetches: they get distinct cache entries and neither is reused for the other. A get with no `params:` hashes exactly as it did before the field existed, so adding this to a pipeline invalidates nothing that does not use it.

The fetched directory is named after the `get:`, so `get: log` puts it in `log/`, and later steps read `log/...`. See [workspace.md](workspace.md) for what a step can and can't see.

### `out` — publish something

Runs when a `put` step executes. Optional: a type with no `out:` is read-only, and a `put:` against it is rejected at load time rather than silently doing nothing.

- **Sees**: `{{ .source }}` and `{{ .params }}` (the put step's `params:`).
- **Working directory** is the put step's read view, composed from its `inputs:`.
- **May print** a single JSON **object** — the version it produced. Printing nothing is fine and not an error. The object is stored as a version, shown on the job page, and rendered into downstream `in:` commands, so print identifiers, not credentials or whole API responses, and quote it with `shellquote`. Over 4 KiB it is not recorded.

### A put publishes; it does not fetch

A put step runs `out:` and nothing else — there is no implicit get afterward, so a put produces no artifact. (Concourse fetches the produced version automatically; steps deliberately does not: an artifact appearing in the build that no step declared is exactly the kind of ambient data flow this DSL rejects.) A plan that wants the resource's contents after a put writes the fetch it means:

```yaml
resource_types:
- name: release
  config:
    check: |
      printf '[{"ref": "v1.4.1"}]'
    in: echo {{ .version.ref | shellquote }} > ref
    out: |
      cat notes/summary.txt        # "publish" the summary an earlier step wrote
      printf '{"ref": "v1.4.2"}'

resources:
- name: releases
  type: release
  source: {}

jobs:
- name: publish
  plan:
  - task: summarize
    outputs: [notes]
    run: echo 'what changed' > notes/summary.txt
  - put: releases              # out: publishes and prints v1.4.2
    inputs: [notes]
  - get: releases              # fetch the resource, explicitly
  - task: verify
    inputs: [releases]
    run: cat releases/ref
    assert:
      # check's answer, NOT the v1.4.2 the put just printed — the get was
      # pinned when the plan was built, before out: ran. See below.
      stdout: v1.4.1
  assert:
    execution: [summarize, releases, releases, verify]   # put, then get — two entries
    outcome: succeeded
```

The explicit get fetches the version `check` reported **when the plan was built** — check runs once, before any step, so the version the put publishes mid-run is not what the same run's get fetches (the example above pins exactly that: `out:` prints `v1.4.2`, the get still fetches `v1.4.1`). The version a put prints is recorded with the run; it reaches a `passed:`-gated get in a later run whether or not a check ever reports it (see [`passed:`](infra.md#a-version-a-job-put-counts-as-having-passed-it)); an ungated get still needs the check to report it. A put whose output nothing reads simply has no get after it.

## Shell safety

Anything interpolated into a command is text substitution, so quote it:

```yaml fragment
check: git ls-remote {{ .source.uri | shellquote }}     # good
check: git ls-remote {{ .source.uri }}                  # a uri with a space or ; breaks or worse
```

`shellquote` renders a value as one safely-quoted shell word. Use it for every `{{ }}` that reaches a command. See [templating.md](templating.md).

Templates render with `missingkey=error`, so reading an optional field that wasn't set fails the render. Ask for optional fields in a way that can answer "nothing":

```yaml fragment
{{ index .source "branch" | default "HEAD" }}     # optional
{{ .source.uri }}                                 # required — failing is correct
```

## `version:` on a get step

By default a get fetches the **latest** version `check` reported. `version: every` runs the rest of the plan once per version, and a mapping pins one exact version:

```yaml
resource_types:
- name: builds
  config:
    check: |
      printf '[{"number": "87"}, {"number": "88"}]'
    in: echo {{ .version.number | shellquote }} > number.txt

resources:
- name: build
  type: builds
  source: {}

jobs:
- name: latest-only
  plan:
  - get: build                     # default: "88", the newest
  - task: show
    inputs: [build]
    run: cat build/number.txt
    assert:
      stdout: "88"
  assert:
    execution: [build, show]
    outcome: succeeded
- name: each-in-turn
  plan:
  - get: build
    version: every                 # the rest of the plan runs per version
  - task: show
    inputs: [build]
    run: cat build/number.txt      # no stdout assert: this runs once per version
  assert:
    execution: [build, show, build, show]   # one run per version, judged together
    outcome: succeeded
- name: pinned
  plan:
  - get: build
    version: { number: "87" }      # exactly this one
  - task: show
    inputs: [build]
    run: cat build/number.txt
    assert:
      stdout: "87"                 # the pin won over the newer version
  assert:
    execution: [build, show]
    outcome: succeeded

assert:
  execution: [latest-only, each-in-turn, pinned]
```

Under `every`, **each version is a run of its own**, as each is a build of its own in Concourse — so `max_in_flight:`, `serial:`, the job's history and **Retry** all count versions. `steps web` builds the oldest version the job has not taken and queues the job again while more wait, so a backlog drains one run per version and, under `max_in_flight: 2`, two at a time. `steps run` builds one version per invocation, as `fly trigger-job` does, and says how many still wait; `steps test` builds every version, each as its own run, and judges the job's `assert:` over all of them, which is why the example above asserts two passes. A failing version does not stop the ones behind it.

### `every` takes each version once

A check reports what *exists*, not what is new — the same twenty Slack messages, the same page of builds, on every poll. So `every` remembers: once a version's run has **started**, that version is not taken again, and a later run takes only what is left. Without that, a plan ending in a `put:` or an `agent:` — the two steps the cache deliberately never skips, because their worth is an effect rather than an artifact — repeats every effect it has ever performed each time anything new shows up.

- **Recorded per (job, resource)**, so another job reading the same resource keeps its own place.
- **A version is taken when its build STARTS**, not when it succeeds — so a version whose build failed is not retried on the next run. This is Concourse's rule (`NextEveryVersion` reads the versions a build was *created* with and never looks at build status), and it is what stops one bad input failing forever, on every trigger, with an agent's bill attached. Re-running it is a deliberate act: `--resume` (the run that took it) or `--pin` (the version), or a new version.
- **`--force` does not lift it**: it skips the step cache only (#145). `steps test` is the exception — it re-opens taken versions so a fixture reruns deterministically against the same state file.
- **It suppresses; [history](#version-history) is what resurrects.** The versions a job may take are the ones steps has recorded, not only the ones `check` returns right now — so a version that scrolled out of the window while nothing was watching is still built. What history does not hold, nothing can recover: a version pruned by `version_history:`, or one from before steps first checked the resource.

- **Any top-level `get:` in a plan may say `every`** — each keeps its own cursor. Anywhere else (inside a hook, or a branch of `in_parallel:`/`do:`/`try:`) the get runs within a build whose versions are already decided, so `every` there is a load error rather than a field that is accepted and ignored. Two `every` gets on the same resource are a load error too — one cursor cannot serve both.
- **A second get of the same resource keeps its own version.** `get: code, version: every` beside `get: baseline, resource: code, version: {ref: "v1"}` fans over `code` while `baseline` stays pinned, which is how a plan diffs what just arrived against a fixed point.

`steps plan` reads the same record, so it lists only the versions a run would actually take.

### Several `every` gets: input sets

When more than one get says `every`, a run resolves **input sets**, Concourse's model: each `every` get advances one step per set through its own unbuilt versions, in lockstep with its siblings, and each set is a run of its own. A get whose versions run out **holds** at the newest version it has already covered while the others keep moving. There is no cross product — 3 new versions on one input and 2 on another mean three builds, not six.

The hold rule is what makes the steady state right, not just the burst: updates rarely arrive in matched pairs. `config` moving alone builds `(code@held, config@new)`; `code` catching up later builds `(code@new, config@held)`.

```yaml
resource_types:
- name: builds
  config:
    check: |
      printf '[{"number": "87"}, {"number": "88"}, {"number": "89"}]'
    in: echo {{ .version.number | shellquote }} > number.txt
- name: configs
  config:
    check: |
      printf '[{"rev": "5"}, {"rev": "6"}]'
    in: echo {{ .version.rev | shellquote }} > rev.txt

resources:
- name: build
  type: builds
  source: {}
- name: conf
  type: configs
  source: {}

jobs:
- name: pairwise
  plan:
  - get: build
    version: every
  - get: conf
    version: every
  - task: show
    inputs: [build, conf]
    # The case pins the pairing, not just the count: a cross product would
    # build (87,6), (88,5) or (89,5) and fail here.
    run: |
      pair="$(cat build/number.txt)-$(cat conf/rev.txt)"
      case "$pair" in
        87-5|88-6|89-6) echo "took $pair" ;;
        *) echo "unexpected pair $pair"; exit 1 ;;
      esac
    assert:
      stdout: took
  assert:
    # Three sets, not six: (87,5), (88,6), then conf is exhausted and
    # HOLDS at rev 6 while build keeps moving — (89,6).
    execution: [build, conf, show, build, conf, show, build, conf, show]
    outcome: succeeded

assert:
  execution: [pairwise]
```

## `trigger: true`

Marks a `get` as something `steps web` should poll. When its version changes, the jobs containing it run automatically. Valid only on `get` steps — setting it anywhere else is a load-time error.

```yaml
resource_types:
- name: ticker
  config:
    check: |
      printf '[{"tick": "1"}]'
    in: echo {{ .version.tick | shellquote }} > tick.txt

resources:
- name: clock
  type: ticker
  source: {}

jobs:
- name: on-change
  plan:
  - get: clock
    trigger: true      # steps web polls this; a new version runs the job
  - task: react
    inputs: [clock]
    run: cat clock/tick.txt
    assert:
      stdout: "1"
  assert:
    execution: [clock, react]
    outcome: succeeded
```

See [infra.md](infra.md) for the watch loop and cross-job triggering, and [webhooks.md](webhooks.md) for a resource whose versions are webhook deliveries. Gating a get on upstream jobs — Concourse's `passed:` — is there too: [infra.md#passed](infra.md#passed--only-run-against-versions-that-are-green-upstream).

## MCP-backed types

A resource type can call an MCP server instead of running shell commands — the same check/in/out roles, as tool calls. See [mcp.md](mcp.md).

## Checking your work

`steps validate pipeline.yml` answers "does it parse and hang together"; `steps plan pipeline.yml` runs `check` and shows what would be fetched vs cached — the fastest way to see whether a `check` you just wrote returns what you expect, since it resolves versions without running the rest of the job.
