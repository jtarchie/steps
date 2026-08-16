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

`git` ships with steps, so that pipeline needs no `resource_types:` block. For anything else, you write the type yourself.

## The built-in `git` type

| `source:` field | required | meaning |
|---|---|---|
| `uri` | yes | anything `git` can clone — https, ssh, or a local path |
| `branch` | no | omitted follows the remote's `HEAD` |

It fetches the exact commit the plan pinned, shallowly, so a branch that moves mid-run still gives you the version that was planned. It has **no `out:`** — `put: repo` against it is a load error, because what "publish" means (which branch, which credentials, force or not) is a decision only you can make. Write your own type for that.

## The built-in `slack-mentions` and `slack-reply` types

Two more built-ins, both [expression-backed](expr.md) — Slack is a JSON HTTP API and nothing else, so there is no container and no `curl`/`jq` dependency to carry. `slack-mentions` is get-only: every unanswered `@mention` of the bot in a channel, plus every message in a 1:1 DM (no `@mention` required there — nobody types one in a 1:1 chat), oldest first, as a `{channel, ts}` version. `slack-reply` is put-only: posts a message, threaded or top-level.

Cold start does not mean a backlog: like every resource, the first-ever check of a freshly-deployed `slack-mentions` records everything it finds but answers none of it — see [Version history](#version-history) below. That rule is a `steps watch` behavior; `steps run` has no persisted cursor and always asks Slack for everything, every time.

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
  - task: address
    inputs: [mentions]
    outputs: [thread, answer]
    run: |
      set -eu
      grep -o '"channel": *"[^"]*"' mentions/version.json | cut -d'"' -f4 > thread/channel
      grep -o '"ts": *"[^"]*"' mentions/version.json | cut -d'"' -f4 > thread/ts
      echo "got it, working on it" > answer/reply.md
  - put: reply
    inputs: [thread, answer]
```

Both need `SLACK_BOT_TOKEN` (a bot token, `xoxb-`) in the environment, for an app with `chat:write`, `channels:history`, `channels:read`, `im:history` and `im:read` (plus `groups:history`/`groups:read` for private channels), installed to the workspace and **invited** to every channel it should watch or post to. Membership is what grants both reading history and appearing in `users.conversations`, so `/invite` *is* the subscribe action — `app_mentions:read` is not needed, since this polls rather than using the Events API.

1:1 DMs are always watched too, with no `@mention` required — there is no `source:` field to turn this off. Group DMs (`mpim`) are not watched at all; a group is closer to a channel (several humans, bot is one more party) than a 1:1, so it keeps the explicit-mention rule instead.

| `source:` field (either type) | required | meaning |
|---|---|---|
| `channels` (`slack-mentions`) | no | `[]` watches every channel (and 1:1 DM) the bot is in; a list of ids narrows it |
| `limit` (`slack-mentions`) | no | per-channel messages fetched per check, default `200` |
| `base_url` (either) | no | overrides `https://slack.com` — for pointing a test at a fake server |
| `token_env` (either) | no | overrides `SLACK_BOT_TOKEN` as the env var name to read the token from |

`token_env` alone isn't enough to widen what a resource can read — `env()` only sees names its resource TYPE already declares (both types declare `SLACK_BOT_TOKEN`, shared by every resource of that type), which is what makes it safe for a shared, possibly-external type to hand-in-hand with any expr type at all. A resource naming a different token also needs `env:` *on the resource itself* to add that name to its own allow-list — `env:` and `source:` together, as in `reply-as-support-bot` above. Naming `token_env` without the matching `env:` entry is a run-time error (`env(...): not in this resource type's env:`), not a silent fall-back to `SLACK_BOT_TOKEN`. (`env:` on a resource only means something for an expr- or shell-backed type — an mcp-backed type authenticates via its `mcp_servers:` entry and rejects `env:` at load time.)

**Two known gaps**, both inherent to a single `ts` cursor plus `conversations.history`'s own `oldest`/`limit` shape — not bugs a bigger `limit:` or a smarter filter closes:

- A reply to a thread whose *parent* message has already scrolled behind the cursor is never seen. The check only re-walks a thread when its parent still comes back from `conversations.history`; once the parent ages past the cursor, a fresh reply to that old thread is invisible forever.
- More than `limit` new messages in one channel between two checks lose the overflow *permanently*, not just delayed — the cursor advances to the newest `ts` seen anywhere, so whatever `limit` cut off now sits below the new cursor and is never asked for again.

Both would need the cursor to carry more than one timestamp (open thread tracking, real Slack pagination) to close — a larger change, not attempted here.

`slack-reply`'s `put:` reads its message from files an upstream step writes, not `params:` — `file()` takes what `inputs:` put on disk directly, so a reply containing backticks or `$(…)` is data, never something a shell might run:

| file | required | meaning |
|---|---|---|
| `thread/channel` | yes | the channel id to post to |
| `thread/ts` | no | a parent message's `ts` — posts as a reply in that thread; omit to post a new top-level message |
| `answer/reply.md` | yes | the message text |

There is deliberately no `check:`/`in:` on `slack-reply` and no `out:` on `slack-mentions` — `get: reply` or `put: mentions` are both load errors, the same rule `git`'s missing `out:` follows.

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

Runs when a plan is built, and on every `steps watch` poll.

- **Sees**: `{{ .source }}` and `{{ .version }}` — the last version this pipeline recorded for the resource. See [the cursor](#the-check-cursor) below.
- **Must print**: a JSON **array** of version objects to stdout, **oldest first**. A version object is a flat map of strings — `{"ref": "abc123"}`, `{"number": "87"}`. The whole object identifies the version; steps never interprets the fields.
- **Empty array** means "no versions yet". Any version mode other than `every` fails the step with `no versions available`. Under `version: every` the job exits 0 having built nothing, and says which kind of nothing it was:
  - `get: <name> cannot build; no versions exist for: <names>` — an input has never had a version at all, so no set can be assembled. The named resources are what to go look at.
  - `get: <name> has no new versions; all N already taken` — the steady state: everything this check reports has been built already.
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

Guess too small and items scroll past during a busy period — and while [history](#version-history) means a version steps already recorded is not lost, one it never saw at all cannot be recovered by anything. Guess anything at all and a cold start reads a backlog nobody is waiting on.

Three things to know:

- **Spell it `{{ index .version "ts" | default "0" }}`, not `{{ .version.ts }}`.** On the first-ever check there is no cursor and `.version` is an empty map; templates render with `missingkey=error`, so the bare form fails that first poll. This is the same shape an optional `source:` field or get `params:` already uses.
- **The cursor belongs to `steps watch` alone**, which both advances it and is the only thing that reads it. A `steps run` or `steps plan` renders `{{ .version }}` as an empty map even when a watcher has been polling for weeks — a manual run asks what exists, not what is new.
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
  records everything it reports and marks it all as already taken, so a
  watcher pointed at twenty existing items answers none of them and waits for
  the twenty-first. (A job *added* to the pipeline later has no such marking,
  and its first trigger will see whatever history holds.)
- **Pruning is not free.** A version dropped from history takes its green
  record with it, so a `passed:` gate can no longer clear for it. That is
  correct — a version out of history cannot be built — but it means a limit
  set below what a slow downstream job needs will hold that job back.

#### How much to remember

`defaults.version_history:` caps it per resource, keeping the newest:

```yaml
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
- **May print** a single JSON **object** — the version it produced. Printing nothing is fine and not an error.

### A put publishes; it does not fetch

A put step runs `out:` and nothing else — there is no implicit get afterward, so a put produces no artifact. (Concourse fetches the produced version automatically; steps deliberately does not: an artifact appearing in the build that no step declared is exactly the kind of ambient data flow this DSL rejects.) A plan that wants the just-published version writes the fetch it means:

```yaml
resource_types:
- name: release
  config:
    check: |
      printf '[{"ref": "v1.4.2"}]'
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
  - put: releases              # out: publishes and prints the version
    inputs: [notes]
  - get: releases              # fetch it back, explicitly
  - task: verify
    inputs: [releases]
    run: cat releases/ref
    assert:
      stdout: v1.4.2
  assert:
    execution: [summarize, releases, releases, verify]   # put, then get — two entries
    outcome: succeeded
```

The explicit get fetches the resource's **latest** version at that moment, like any other get — for almost every plan that is the version the put just published, but a concurrent publisher can race it. A put whose output nothing reads simply has no get after it.

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
    execution: [build, show, build, show]   # the fan-out, one pass per version
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

Under `every`, a failing version does not stop the remaining ones from being attempted.

### `every` takes each version once

A check reports what *exists*, not what is new — the same twenty Slack messages, the same page of builds, on every poll. So `every` remembers: once a version's fan-out has **succeeded**, that version is not taken again, and a later run fans out only over what is left. Without that, a plan ending in a `put:` or an `agent:` — the two steps the cache deliberately never skips, because their worth is an effect rather than an artifact — repeats every effect it has ever performed each time anything new shows up.

- **Recorded per (job, resource)**, so another job reading the same resource keeps its own place.
- **A version is taken when its build STARTS**, not when it succeeds — so a version whose build failed is not retried on the next run. This is Concourse's rule (`NextEveryVersion` reads the versions a build was *created* with and never looks at build status), and it is what stops one bad input failing forever, on every trigger, with an agent's bill attached. Re-running it is a deliberate act: `--force`, `--resume`, or a new version.
- **`--force` ignores it**, along with every other piece of persisted state — which means it re-runs versions already taken, effects included. It still *records* what it took, so the ordinary run after a forced one does not repeat that work a second time.
- **It suppresses; [history](#version-history) is what resurrects.** The versions a job may take are the ones steps has recorded, not only the ones `check` returns right now — so a version that scrolled out of the window while nothing was watching is still built. What history does not hold, nothing can recover: a version pruned by `version_history:`, or one from before steps first checked the resource.

- **Any top-level `get:` in a plan may say `every`** — each keeps its own cursor. Anywhere else (inside a hook, or a branch of `in_parallel:`/`do:`/`try:`) the get runs within a build whose versions are already decided, so `every` there is a load error rather than a field that is accepted and ignored. Two `every` gets on the same resource are a load error too — one cursor cannot serve both.
- **A second get of the same resource keeps its own version.** `get: code, version: every` beside `get: baseline, resource: code, version: {ref: "v1"}` fans over `code` while `baseline` stays pinned, which is how a plan diffs what just arrived against a fixed point.

`steps plan` reads the same record, so it lists only the versions a run would actually take.

### Several `every` gets: input sets

When more than one get says `every`, a run resolves **input sets**, Concourse's model: each `every` get advances one step per set through its own unbuilt versions, in lockstep with its siblings, and one build runs per set. A get whose versions run out **holds** at the newest version it has already covered while the others keep moving. There is no cross product — 3 new versions on one input and 2 on another mean three builds, not six.

The hold rule is what makes the steady state right, not just the burst: updates rarely arrive in matched pairs. `config` moving alone builds `(code@held, config@new)`; `code` catching up later builds `(code@new, config@held)`.

```yaml
resource_types:
- name: builds
  config:
    check: |
      printf '[{"number": "87"}, {"number": "88"}]'
    in: echo {{ .version.number | shellquote }} > number.txt
- name: configs
  config:
    check: |
      printf '[{"rev": "5"}]'
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
    run: echo "$(cat build/number.txt) with conf $(cat conf/rev.txt)"
  assert:
    # Two sets: (87, 5) and (88, 5) — conf is exhausted after the first
    # set and HOLDS at rev 5 while build keeps moving. Not a cross product.
    execution: [build, conf, show, build, conf, show]
    outcome: succeeded

assert:
  execution: [pairwise]
```

## `trigger: true`

Marks a `get` as something `steps watch` should poll. When its version changes, the jobs containing it run automatically. Valid only on `get` steps — setting it anywhere else is a load-time error.

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
    trigger: true      # steps watch polls this; a new version runs the job
  - task: react
    inputs: [clock]
    run: cat clock/tick.txt
    assert:
      stdout: "1"
  assert:
    execution: [clock, react]
    outcome: succeeded
```

See [infra.md](infra.md) for the watch loop, webhooks, and cross-job triggering.

## MCP-backed types

A resource type can call an MCP server instead of running shell commands — the same check/in/out roles, as tool calls. See [mcp.md](mcp.md).

## Checking your work

`steps validate pipeline.yml` answers "does it parse and hang together"; `steps plan pipeline.yml` runs `check` and shows what would be fetched vs cached — the fastest way to see whether a `check` you just wrote returns what you expect, since it resolves versions without running the rest of the job.
