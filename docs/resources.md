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
- **Empty array** means "no versions yet". Under `version: every` the get fans out zero times and the job exits 0, so steps prints `get: <name> returned no versions; the N step(s) after it did not run` to say how much of the plan that dropped; any other version mode fails the step with `no versions available`. The message tells you a check came back empty — it cannot tell you *why*, so a type should still **fail loudly** (exit non-zero) when it can't answer, rather than printing nothing.
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

Guess too small and items scroll past during a busy period, permanently — steps keeps no version history, so whatever the window missed is gone. Guess anything at all and a cold start replays a backlog nobody is waiting on.

Three things to know:

- **Spell it `{{ index .version "ts" | default "0" }}`, not `{{ .version.ts }}`.** On the first-ever check there is no cursor and `.version` is an empty map; templates render with `missingkey=error`, so the bare form fails that first poll. This is the same shape an optional `source:` field or get `params:` already uses.
- **Only `steps watch` advances it**, and only after the jobs a version implies are enqueued — so a poll that finds versions and then dies does not skip past them. `steps run` and `steps plan` read the cursor and never move it; a run that moved it would push the watcher's baseline past a version no watch loop ever acted on.
- **A check that ignores `.version` keeps working exactly as before.** The cursor narrows what a check *asks for*; it is not a filter steps applies to the answer.

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

By default a get fetches the **latest** version `check` reported. `version: every` runs the rest of the plan once per version — the only fan-out point in a plan — and a mapping pins one exact version:

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
- **`--force` ignores it**, along with every other piece of persisted state — which means it re-runs versions already taken, effects included. It still RECORDS what it takes, so an ordinary run afterwards does not repeat the same work again. It still *records* what it took, so the ordinary run after a forced one does not repeat that work a second time.
- **It can only suppress, never resurrect.** steps keeps no version history: whatever `check` returns *now* is the whole universe. A version that scrolled out of the check's window while nothing was watching is gone. The [check cursor](#the-check-cursor) is how a type stops that being a guess — a check told where it left off can ask for everything since, rather than hoping a fixed window was wide enough.

- **Only the first `get:` in a plan may say `every`.** That is the one fan-out point; a later get runs *inside* one of those fan-outs, where it can only fetch a single version. Writing it there is a load error rather than a field that is accepted and ignored. (Concourse allows several — each input has its own cursor — because it resolves a whole input *set* per build instead of fanning out at one step. See [conformance](conformance.md).)

`steps plan` reads the same record, so it lists only the versions a run would actually take.

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
