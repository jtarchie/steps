# Expression resource types

A resource type is [three shell commands](resources.md#writing-a-resource-type). For a resource that is a JSON HTTP API, that costs about forty-five lines of `curl` and `jq` — and most of those lines have nothing to do with the API. They work around the shell.

`expr:` is a second way to write the same three stages, as **expressions** instead of commands.

## When to use it

> Reach for `expr:` when the resource is a JSON HTTP API and nothing else. Reach for `check:`/`in:`/`out:` for everything else.

The rule is narrow on purpose. Shell is not deprecated and is not going anywhere — it is the only option, not merely the traditional one, when:

- **a real tool does the work** — `git`, `docker`, `aws`, `kubectl`
- **the artifact is binary**, or big enough that you would rather stream it to disk than hold it as a string (expr is text-only)
- **the command should run in a container** — `image:`, `network:`, `privileged:` and `container_limits:` are shell-only, because an expression evaluates in-process

What you get in exchange, on the JSON-API case:

- **No interpreter dependency.** No `curl`, no `jq`, nothing to install on the machine that polls.
- **No quoting hazard.** Values are values; nothing is spliced into a command as text, so a `shellquote` you forgot cannot become an injection.
- **Errors that propagate.** A shell loop piped into `jq` throws away its exit status, so one dead request silently shrinks the version list and reads as "no new versions". That is the failure mode the `mktemp`/`trap` dance in hand-written checks exists to work around.
- **Concurrent, rate-limit-aware I/O.** `http()` takes a list of requests and steps owns the fan-out — see below.

A resource type picks **exactly one** backend. Mixing `expr:` with `check:`/`in:`/`out:` or with `mcp:` is a load error.

## The three stages

```yaml
resource_types:
- name: counters
  config:
    expr:
      # Returns an array of version objects, OLDEST FIRST — the same contract
      # a shell check's stdout has, and the same responsibility for ordering.
      check: |
        1..source.count | map((
          {n: string(#)}
        ))

      # Returns an object of artifact-relative path to file contents. steps
      # writes them into the get's directory.
      in: |
        {
          "version.json": toJSON(version),
          "n.txt": version.n,
        }

resources:
- name: ticker
  type: counters
  source:
    count: 3

jobs:
- name: build
  plan:
  - get: ticker
  - task: show
    inputs: [ticker]
    run: cat ticker/n.txt
    assert:
      stdout: "3"          # check returned oldest-first, so latest is last
  assert:
    execution: [ticker, show]
    outcome: succeeded
```

| Stage | Sees | Returns |
|---|---|---|
| `check` | `source`, `version` | an array of version objects, oldest first |
| `in` | `source`, `version`, `params` | an object of relative path → file contents |
| `out` | `source`, `params`, and `file()` | the version it published, or `nil` |

`check`'s `version` is the [check cursor](resources.md#the-check-cursor): the last version `steps watch` recorded, so a poll can ask for what it has not seen. It is an **empty map** on the first-ever poll — and under `steps run`/`steps test`, which never receive a cursor — which is what makes `version.ts ?? "0"` both the natural spelling and the correct one rather than an incantation.

`in` returns a file map rather than getting a `write()` builtin. That keeps an expression pure — no side effects, so its result is a function of its inputs, which is what the artifact cache is already built on. Paths must be relative and stay inside the artifact directory. Omit `in` entirely and steps writes `version.json` alone, which is all a type that merely detects change needs.

`out` returns the version it produced, which steps records as what the put published. Returning `nil` is fine — plenty of publishing produces nothing versionable.

## Builtins

Everything else is [expr's own standard library](https://expr-lang.org/docs/language-definition): `map`, `filter`, `find`, `flatten`, `sortBy`, `reduce`, `concat`, `len`, `take`, `uniq`, `first`, `last`, `toJSON`, `fromJSON`, `float`, `int`, `string`, `trim`, `keys`, `values`, `get`.

### `env(name)` / `env(name, default)`

Reads an environment variable, and **only** the names the resource type lists in `env:`. There is no baseline allowlist beneath that: a shell command inherits `PATH` and `HOME` because it goes on to run real tools, and an expression runs nothing.

An allowed-but-unset variable is an **error**, not an empty string — an unset token otherwise sends an unauthenticated request that fails later, somewhere else, as something else. Pass a default to opt into that: `env("TOKEN", "")`.

### `file(path)` / `file(path, default)`

`out` only. Reads from the put's inputs — the same directory a shell `out:` gets as its cwd. Relative paths only. Files usually end in a newline, so `trim(file("thread/ts"))` is the common spelling.

### `fail(message)`

The only way an expression can refuse. It exists because "the request
succeeded and the API said no" is the normal shape of a JSON API — Slack
answers `200` with `{"ok": false, "error": "not_in_channel"}` — and `http()`
deliberately treats a status as data. Without it, an `out:` whose post was
rejected returns `nil`, which is indistinguishable from a put that
legitimately published nothing: green, having done nothing.

Paired with the ternary (which short-circuits), it reads as a guard:

```yaml fragment
out: |
  let posted = http({url: "…", json: {…}}).json;
  posted.ok ? {channel: posted.channel, ts: posted.ts} : fail("slack: " + posted.error)
```

### `http(request)` / `http(requests, settings)`

**`http()` takes a list of requests**, and a single request is sugar for a one-element list. Nearly all of this backend's leverage lives in that one decision.

```yaml fragment
http([{url: "…", method: "POST", query: {…}, headers: {…}, json: {…}}, …],
     {headers: auth, concurrency: 4, retry: {on: [429, 503], max: 3}})
# -> [{request: {…}, status: 200, headers: {…}, json: {…}, body: "…", error: nil}, …]
#    in REQUEST order
```

A check against a chat API is `1 + N` round trips, and in shell every one is serial. Expr cannot express concurrency at any price, and shell can only get it with background jobs and `wait` — which is exactly the construct that loses exit status. Batching is what makes concurrency safe to offer: steps owns the fan-out, so steps owns the error handling, the rate limiting, and the attribution.

**Request keys**: `url` (required), `method` (default `GET`), `query`, `headers`, `json` (marshaled, implies `POST` and sets the content type), `body` (raw string). An unknown key is an error rather than ignored — a misspelled `header:` that silently sends no authorization surfaces as a 401 somewhere else entirely.

**Settings**: `headers` (merged into every request; a request's own header wins), `concurrency` (default 4), `timeout` (default `"30s"`, per request), `max_response_bytes` (default 8 MiB), `retry`, `tolerate_errors`. A setting spelled wrong is an error, and so is one **typed** wrong — `retry: {on: ["429"]}` would otherwise read as configured and retry nothing.

Shared headers are a *setting* rather than something you merge into each request because **expr has no `merge()`** — so the API is shaped to make merging unnecessary.

**The envelope is uniform**, including for a single request: `.json`, never a bare parsed body. Slightly more to type, no special case to remember when the call later becomes a batch. `request` is your request echoed back, which is what lets an expression recover `#.request.query.channel` instead of zipping two arrays by index.

**A status is data.** Non-2xx does not fail the call — an API that answers `200` with `ok: false`, or `404` for something that does not exist yet, is answering. Only a request that never produced a response is a failure, and that fails the whole `http()` call with a message naming the method and URL. That is the point: a check that cannot reach its API should fail loudly, not return a shorter list that reads as "nothing new".

**Retries** apply only to the statuses you list. `Retry-After` is honored when the server sends one (seconds or an HTTP date, capped at a minute), otherwise the backoff is exponential. When retries run out the **last response is returned** rather than an error — a persistent 429 is something your expression gets to decide about.

**Partial tolerance** is opt-in. `{tolerate_errors: true}` turns a failed request into an envelope with `status: 0` and an `error` string instead of failing the batch, so one channel a bot was removed from does not take out a poll over nineteen healthy ones:

```yaml fragment
http(requests, {tolerate_errors: true})
  | filter(#.error == nil)
  | map((
    {…}
  ))
```

## Gotchas

These are the ones that will bite you first. They are properties of expr itself, not of steps.

- **A map literal as the first thing in an argument needs an extra pair of parentheses.** `map({a: #})` does not parse; `map(({a: #}))` does. This is by far the most common surprise, and it applies anywhere a `{` opens an argument.
- **Comments are `//` and `/* */`.** A `#` starts a *pointer*, not a comment, so a `#`-commented line is a syntax error in the middle of your program.
- **`concat(a, b)` joins two lists.** `+` is arithmetic and refuses them.
- **Nested predicates shadow `#`.** Capture the outer value with `let` first — `let ch = #.request.query.channel; …` — which the spec requires and is not stylistic.
- **`not in` is not a thing.** Write `!("subtype" in #)`.
- **`toJSON` indents.** Valid JSON, just not compact.
- **There is no `try`/`catch`, and no way to add one.** Deferring an expression means compiling it separately, and a separately compiled program cannot see `let` bindings or `#` — both live on the VM's stack. So tolerance lives where the errors are born instead: `tolerate_errors` on `http()`, and the optional second argument to `env()` and `file()`.
- **`now()`, `date()` and `duration()` are unavailable in `check` and `in`.** A check calling `now()` returns a different "version" on every poll, and a version has to be a stable identity — that is a pipeline that re-runs forever with whatever an agent step costs attached. `in` is barred for the neighbouring reason: its output is cached across builds, so it must be a pure function of the version it fetches. `out` may still use them.

`??`, `in`, `contains`, `matches`, `..`, `?.`, `#index` and `#acc` are all real and all useful.

## Pagination is a `reduce`, not a builtin

Expr has no loops, but `reduce` threads an accumulator — so a cursor walk is a reduce over a **bounded** range with an early-out. The page cap is the range itself, visible in the source, which auto-following a `next_cursor` would not be:

```yaml fragment
check: |
  reduce(1..10,                                    # the range IS the max-pages cap
    #acc.done ? #acc : (
      let page = http({url: source.url, query: {cursor: #acc.cursor}}, {headers: auth}).json;
      let next = page.response_metadata.next_cursor ?? "";
      {cursor: next, items: concat(#acc.items, page.items), done: next == ""}
    ),
    {cursor: "", items: [], done: false}
  ).items
```

**The `#acc.done` guard is load-bearing.** Without it, `reduce` keeps calling `http()` past the end of the results — ten pages of requests for three pages of data.

Offset- and page-number APIs are simpler *and* better: the page numbers are known up front, so they go out as one batched concurrent `http()` call rather than a serial chain.

## A real one

Answering mentions in a chat workspace: resolve who we are, list the channels we are in, then fetch each channel's history since the cursor — three dependent calls and a fan-out. `noexec` because it reaches a real host.

Slack specifically ships as a built-in — `type: slack-mentions` / `type: slack-reply` need no `resource_types:` block at all, and cover more than this sketch does (1:1 DMs, thread replies, multiple bots in one pipeline). See [Resources](resources.md#the-built-in-slack-mentions-and-slack-reply-types). What follows is kept as a worked example of the *shape* — resolve identity, discover, fan out, filter — for a chat API that isn't Slack.

```yaml noexec=network
resource_types:
- name: mentions
  env: [BOT_TOKEN]
  config:
    expr:
      check: |
        let auth = {Authorization: "Bearer " + env("BOT_TOKEN")};

        let me = http({url: "https://slack.com/api/auth.test"}, {headers: auth}).json.user_id;

        let channels = len(source.channels) > 0 ? source.channels :
          http({url: "https://slack.com/api/users.conversations",
                query: {types: "public_channel", limit: "1000"}},
               {headers: auth}).json.channels | map((
            #.id
          ));

        // One batched call instead of N serial ones: concurrent, backing off
        // politely on a 429, and each result carries the request that made it.
        http(channels | map((
              {url: "https://slack.com/api/conversations.history",
               query: {channel: #, oldest: version.ts ?? "0", limit: string(source.limit)}}
            )),
            {headers: auth, concurrency: 4, retry: {on: [429, 503], max: 3}})
        | map((
            let ch = #.request.query.channel;
            #.json.messages
              | filter(!("subtype" in #) && !("bot_id" in #) && #.user != me)
              | filter((#.text ?? "") contains "<@" + me + ">")
              | map((
                {channel: ch, ts: #.ts}
              ))
          ))
        | flatten()
        | sortBy(float(#.ts))

resources:
- name: asks
  type: mentions
  source:
    channels: []          # [] = every channel the bot is in
    limit: 200

jobs:
- name: answer
  plan:
  - get: asks
    trigger: true
    version: every
```

Note `version.ts ?? "0"`: that is the [cursor](resources.md#the-check-cursor), and it is why there is no `limit: 20` guess here. And `sortBy(float(#.ts))` — a Slack `ts` is a *string*, so sorting it as one silently misorders.

## Expressions belong in files

A twenty-line program has no business inside a YAML scalar, so each slot takes a `_file` sibling — the pattern `run_file:`, `system_file:` and `message_files:` already establish:

```yaml fragment
resource_types:
- name: mentions
  env: [BOT_TOKEN]
  config:
    expr:
      check_file: types/slack/check.expr
      in_file:    types/slack/in.expr
      out_file:   types/slack/out.expr
```

Paths are relative to the pipeline file, and the contents are inlined before anything is validated or hashed — so a `_file` type behaves identically to one written inline, and editing the file re-runs the steps that depend on it. Setting both `check:` and `check_file:` is an error rather than one silently winning. Suggested extension `.expr`.

Beyond legibility: a real file is reviewable. A diff reads as a diff, and a comment lands on a line instead of on a blob of YAML.

## What it cannot do

Stated plainly rather than discovered:

- **No containers.** `image:`, `network:`, `privileged:` and `container_limits:` alongside `expr:` are load errors. An expression evaluates in this process; there is nothing to isolate. Per-call `timeout` and `max_response_bytes` on `http()` are what replace them.
- **Text only.** Binary artifacts are shell's job.
- **No shell, ever.** There is no builtin that runs a command, and expr has no way to reach one. That is the safety property, not an omission.
- **Syntax errors are not load errors.** `steps validate` and `steps validate --live` catch them — both before anything polls — but `steps` will parse a pipeline containing a broken expression. The config layer deliberately has no expression engine in it.
