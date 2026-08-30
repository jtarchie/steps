# The web UI

```bash
steps web pipeline.yml
```

Serves a browser view of what the runner has done and is doing, at
`http://127.0.0.1:8088` — and, unless told not to, polls `trigger: true`
resources while it serves, so one command both notices new versions and builds
them. Several pipelines at once:

```bash
steps web app.yml infra.yml nightly.yml
```

Each is routed under `/p/<name>/`, where the name is the YAML's base name
unless `--name` says otherwise. By default each pipeline gets its own
`.steps/<filename>.db`, so two never share a database — not even two sitting in
one directory. `--state` is how you ask them to; see
[One database, several pipelines](#one-database-several-pipelines).

## What it shows

| Route | Answers |
|---|---|
| `/` | With several pipelines served: what this process holds, and one run feed across all of them, newest first. With one, it redirects straight through |
| `/p/:pipeline` | Which jobs exist, how each last run went, and which jobs feed which — list or `git log`-style graph |
| `…/jobs/:job` | This job's dependencies in both directions, its run history with a duration trend, the resource versions it has passed against, and the resolved limits each agent step runs under |
| `…/runs/:run` | **The transcript**: every step in plan order, what it did, and — for agent steps — what the model said and which tools it called |
| `…/nodes/:hash` | What a merkle hash is made of, and every run that reused it: the cache's receipt |
| `…/approvals` | Pending `approval:` steps, and the decisions already made |
| `…/questions` | Pending `ask_user` questions, and the answers already given |
| `…/resources` | Latest checked version per resource, and any job the circuit breaker has paused |
| `/docs` | These docs, rendered with syntax-highlighted examples — the same pages `steps docs` shows in a terminal |

Press `/` anywhere for a jump palette over pipelines, jobs, and recent runs — across **every** pipeline this process serves, not only the one whose page you are on. The one you are on ranks first, and a hit from anywhere else says which pipeline it belongs to.

### Agent dials

A job page lists the **resolved** limits of each agent step in its plan: turns, context ceiling, and deadline, after the step, the agent and the built-in default have all had their say. It exists so "why did this step stop at 30 turns" is answerable without cross-referencing three files, and it shows `uncapped` rather than `0` for a dial an author explicitly removed — `0` in a limit column reads as the opposite of what it means.

It covers the agents a *step* names. A task's `fix:` agent and a step's sub-agent `tools:` grants run under limits of their own and are not listed.

## The transcript

The run page is the point of the whole thing. It renders a run the way the
terminal does — steps in order, prefixed by kind, colored by outcome — with
the things a scrollback cannot give you:

- **A step that stopped early says so.** An agent whose turn budget ran out is asked to answer from what it already gathered, and the answer is *degraded* — afterwards it is indistinguishable from a confident one unless the record says otherwise. It carries a `stopped early` badge, live and on reload alike. Its neighbour on the spend panel answers the other half: a response cut off mid-sentence by the model's own output limit.
- **A plan is a tree, and it renders as one.** A block step (`across:`,
  `in_parallel:`, `race:`, `ensemble:`, `do:`, `try:`) holds the steps that ran
  inside it, indented under a guide rail, and folding the block folds its whole
  subtree. A triggering `get:` holds the entire build its version set off,
  which is what it already is — the get does not finish until that build does.
- **A folded block still says where it stands.** Its row carries the count of
  what is inside it — `3 steps · 2 passed · 1 failed` — because the rows that
  would otherwise answer are folded away with it. A block wrapping a single
  step carries no count unless that step failed: one child says nothing its own
  row does not.
- **The rail lights along the branch that is working.** While a run is in
  flight, every block holding something still running is marked, so a reader
  who has folded half the page still knows where to look. A failed branch is
  not marked the same way — the counts above already carry the failure up every
  ancestor.
- **Keyboard**: <kbd>j</kbd>/<kbd>k</kbd> walk the tree, <kbd>e</kbd>/<kbd>c</kbd>
  expand and collapse everything, <kbd>enter</kbd> toggles, and <kbd>f</kbd>
  jumps to the innermost failure — the step that actually broke, rather than
  the outermost block that reports it.
- **A cached step is folded and dimmed**, labeled `unchanged — replayed from
  cache`. The steps a chain-skip swallowed are shown too, for the same reason:
  a transcript that stops at the cache hit reads as a truncated run rather
  than a cheap one.
- **A failed run leads with the error**, and says what changed since the last
  green run of that job — computed by comparing content hashes, so it names
  the steps whose inputs, command, or prompt actually moved.
- **A task step expands into what it printed.** Output is captured while it
  still streams to the terminal and bounded at 16KB per step. Recorded
  whichever way the step ended, and especially when it failed: the error a
  task returns names the exit status, and an assert mismatch names the
  expectation — neither carries the output that explains either. A step that
  printed nothing is not expandable at all, so a chevron never promises detail
  that is not there.
- **An agent step expands into its conversation**: the model's text each turn,
  every tool call, and any sub-agent delegation nested underneath, closing with
  the step's `answer`.
- **Every payload is rendered as JSON, not printed.** A call's arguments, a
  tool result, a node's content map, a resource version: parsed and
  highlighted, in the order they were recorded. A document that arrived escaped
  inside a string — `read_file` returns `{"content": "…"}`, and that content is
  often JSON itself — is shown as the document it is, unescaped, still inside
  its quotes so the nesting stays legible. Small payloads sit on the row; a
  bulky one folds behind a summary that names what is inside it (`content ·
  555 B`), so one 30KB tool result cannot bury the conversation around it.
- **An agent's answer is rendered as markdown**, because that is what models
  write: headings, lists, tables, emphasis. A ```` ```json ```` block is
  highlighted exactly like the tool results above it; every other language goes
  through the same highlighter `/docs` uses.

  The text came from a model, so the renderer is a deliberately narrowed one.
  Raw HTML is dropped rather than parsed. A `javascript:` or `data:` link
  renders with no destination, and every surviving link carries
  `rel="noopener noreferrer nofollow"`. **Images are never fetched** — an
  `![](http://…)` in a review is a request the browser makes on its own, so the
  page shows the alt text and the host it wanted instead. Headings mint no
  anchor ids, which are the page's own (`#step-…`) to hand out.
- **A CLI-backed agent reads the same as a hosted one.** An agent whose
  `source.model:` is a CLI (`@claude/sonnet`) runs its own tool loop in a
  subprocess, so steps reads that subprocess's transcript as it streams and
  publishes the same turns: the model's text, every tool call, and every
  result. They appear live while the step runs and are stored with it
  afterwards, so the node page's conversation works for a CLI step too.
- **What a step cost**, when anything reported it. A CLI meters itself and
  prints a figure when it exits; the HTTP paths report tokens and leave pricing
  to whoever knows the rate card. A run where nothing reported one says
  `unpriced` rather than `$0.00`, which would read as free, and a run where
  only some steps did says `$0.42+3?` — a bill for three of six steps
  presented as the whole one is the same lie in the other direction.
- **Which machines the run used**, on a `machines` panel beside the spend one,
  for a run that placed any step: the tag, the platform the worker reported,
  the filesystem the tree landed on and the space left there, how many bytes
  had to be pushed to it, the identity it ran as, and the machine — plus the
  image if the step ran in a container on it. A `tmpfs` workdir is marked in
  warning colour, because it is *memory* and the reader is scanning for
  exactly that. A worker that could not report a filesystem reads `not
  reported` rather than a blank that looks like an ordinary disk, and a shim
  that named no identity leaves the cell empty rather than inventing `0:0`,
  which would read as root.

  There is deliberately **no cost column**: what an instance-hour actually
  cost is not knowable from inside a run — list prices ignore Savings Plans
  and Reserved Instances, a spot instance's paid price is reported by no API,
  and real billing lands up to a day later. That is the opposite call from
  spend above, where the *provider* reports the dollars and steps only records
  what it was told. `steps runs where <pipeline>` reads the same rows in a
  terminal.
- **Every hash is a link** to the node page, and **every step has one too** —
  the `#` beside its name is a URL you can paste at someone, and it opens the
  step it names.

## Live runs

A run still in flight streams to the page over server-sent events: steps
appear as they start, tool calls arrive as the model makes them, and the page
settles into its final state when the run ends.

The stream is built on the same rows the finished-run page reads, so a run
watched live and the same run opened an hour later show the same thing, and a
dropped connection costs nothing but a reconnect. It also means the UI shows
runs it did not start — a `steps run` in another terminal against the same
pipeline appears here as it happens.

Recording is the runner's job, not the UI's: **every** run persists its events
(`run_events`), whether or not anything is watching. A job started from a
terminal leaves the same record as one started from the browser.

## Following a run you started

Triggering does not drop you back on a list to refresh. A trigger lands on a
short waiting page that reports what the queue is doing and forwards itself to
the live transcript the moment a worker picks the job up — a queued job has no
run id until then, which is why there is a waiting room rather than a
redirect.

While a run is live, the browser tab carries its status: `◐` running, `✓`
passed, `✗` failed, with a matching favicon dot. The title updates the instant
the run ends, so a run left in a background tab reports its outcome without
being reopened.

The jobs board refreshes itself every couple of seconds, in place — it keeps
your list/graph choice and scroll position rather than reloading the page —
and pauses while the tab is hidden.

## Triggering, approving, resuming

Four controls, each writing the same rows the CLI writes:

- **Trigger** / **Re-run (forced)** enqueue the job into the durable trigger
  queue `steps web` uses — the same queue this process's own polling fills.
  `steps web` drains it in-process by calling `pipeline.RunJob` — there is no
  second execution path, so a job run from a browser gets the same caching,
  hooks, serial groups, and recording as any other. Forced re-run skips the merkle cache; an unforced one does not,
  which on an unchanged pipeline correctly does almost nothing.
- **Approve / Reject** on an `approval:` step, with the reason recorded — the
  same row `steps approvals approve` writes.
- **Answer** an `ask_user` question a step is parked on — one click for an
  offered option, or your own words — the same row `steps questions answer` writes. See
  [agents.md](agents.md).
- **Resume** a job the trigger circuit breaker paused.

`--read-only` withholds all four: the controls disappear from the pages and
the routes refuse. The queue is still drained, and polling still runs — that
flag is a statement about the browser's surface, not about what the process
does on its own. `--listen 0.0.0.0:8088 --read-only` is a build box that still
has to notice new versions.

**The webhook route is the one exception, deliberately.**
`POST /p/<slug>/check/<resource>` still works under `--read-only`, and the job
it enqueues still runs. It is not a UI control: it carries the resource's own
token, which is a stronger check than the four above have, and withholding it
would mean a read-only box could not be the thing GitHub notifies — which is
most of why a build box is exposed at all. `--read-only` says a *browser*
cannot start work here; it does not say nothing can. If that is what you
want, do not give the pipeline a `webhook_token_env:` resource — with none, the
route is a 404. See [infra.md](infra.md#webhook-triggered-checks).

## One daemon

`steps web` is the whole long-running mode: it serves the UI, polls every
`trigger: true` resource, and drains the queue both of those fill. There is no
separate watcher — a front end that drains a queue nothing fills is a runner
that looks alive and notices nothing, and two processes against one state
database claim each other's work.

```bash
steps web pipeline.yml                      # serve, and poll every 30s
steps web pipeline.yml --interval 5m        # slower
steps web pipeline.yml --once               # poll once, run what that triggers, exit
steps web pipeline.yml --max-concurrent 4   # up to four queued jobs at a time
```

`--once` is the cron form: one poll, drain until the queue is empty, exit —
**without binding the listen address**, because a port opened for the duration
of one poll is a port nothing has time to reach. It is what a systemd timer or
a CI step drives when the schedule already belongs to something else.

- **One poller per pipeline.** Within one pipeline the poller is handed the
  store handle its drain already uses rather than opening a second one.
  `trigger.Poll`'s doc comment has the why; the short version is that a store
  is a single pooled connection, so sharing it queues the poll's writes behind
  the drain's instead of adding a second connection to fight for the same
  write lock.
- **One `steps` process per state database, and nothing enforces it.** Two of
  them claim each other's work, and startup recovery — re-queueing rows a
  crashed process left claimed — reads every `running` row as abandoned, which
  is true only when nothing else is alive. A second `steps web` against one
  state file is a deployment mistake, not a supported pairing.
- **A pipeline with no `trigger: true` get is not an error.** It is noted in
  the log and served anyway, because plenty of pipelines are run by hand and
  the UI is where you would run them from. Under `--once` it polls nothing and
  exits.
- **Preflight runs before the first poll**, the same check `steps web` does
  and with the same asymmetry: a problem *waiting cannot fix* — an `mcp:` tool
  the server does not expose — stops that pipeline's polling and says so,
  while a problem waiting might fix — a server that did not answer, a token a
  refresh would renew — is printed once as `(transient — polling anyway)` and
  left to the loop, which retries by its nature. `--no-preflight` skips the
  check entirely. It runs inside the poller, so it never delays serving.

## One database, several pipelines

`--state` points any command at a specific sqlite file, and several pipelines
may share one:

```bash
steps web app.yml infra.yml --state /var/lib/steps/state.db
steps run app.yml --job deploy --state /var/lib/steps/state.db
```

One file to back up, and one file to delete. What it is *not* is a merge:
inside the database every row carries the pipeline it belongs to, so histories,
resource versions, queues, serial groups and the merkle cache stay separate.
Two pipelines each with a job named `build` running an identical task do not
share a cache entry, and one pipeline's `run_history:` cap never reaps
another's runs.

Reading one back needs no pipeline argument. `steps runs --state <file>` lists
what the file holds and interleaves the newest runs of all of it, which is the
terminal's version of the web root:

```bash
$ steps runs --state /var/lib/steps/state.db

PIPELINE  PATH
app       /src/app/pipeline.yml
infra     /src/infra/pipeline.yml

WHEN                 PIPELINE  JOB      STATUS     RUN
2026-08-30 09:14:02  infra     deploy   succeeded  UNVFHMCHVWY6GV6N
2026-08-30 09:12:40  app       build    failed     46UMHVPYRA6YHB7M
```

The `RUN` column is the handle for going back to one pipeline: `steps runs
cost app.yml 46UMHVPYRA6YHB7M --state <file>`. That is also why the other
views stay scoped — `runs steps`, `runs queue`, `runs cost` and `runs where`
are questions about one pipeline, and each takes it as its first argument
rather than being answered for a pipeline nobody picked.

A pipeline's identity in the file is its **name**, which defaults to the YAML's
base name — `infra/pipeline.yml` is `pipeline`. That is also its `/p/<name>/`
route. When two files would claim one name, `--name` settles it:

```bash
steps web app/pipeline.yml infra/pipeline.yml --state shared.db \
  --name app=app/pipeline.yml --name infra=infra/pipeline.yml
```

The name is the identity, not the path — so a checkout that moves keeps its
history, and renaming the YAML (or changing `--name`) starts a new pipeline
with empty state. Nothing in a content-addressed cache can tell a rename from a
different pipeline, so this is the honest reading rather than a limitation.

Run ids stay globally unique, but `--resume` and `--replay` still refuse an id
belonging to a different pipeline in the same file: continuing another
pipeline's run would reuse its workspace and step indexes against this
pipeline's plan.

There is no migration path. A database written by a different schema is refused
on open with a message saying so; the answer is to delete the file, which costs
run history and cache and nothing else.

## Security

There is no authentication, because there is nothing to authenticate against:
this is the local runner's own front end, in the same trust domain as the
shell that started it. It binds `127.0.0.1` by default.

Mutations are POST-only and require a same-origin `Origin` header when one is
present, so another page cannot aim a form at your localhost port.

**Binding to a routable address publishes trigger and approval controls to
anyone who can reach the port.** `--listen 0.0.0.0:8088` exists for someone
who has decided that is what they want; pair it with `--read-only` unless you
mean to hand out the controls too.

## Flags

```
--listen         address to serve on (default 127.0.0.1:8088)
--interval       how often to poll trigger: true resources (default 30s)
--once           poll once, run what that triggers, exit without serving
--max-concurrent maximum queued jobs running at once, per pipeline (default 1)
--pin / --force  pin a version field; ignore the cache and re-run every step
--no-preflight   skip the pre-poll health check of models and MCP servers
--read-only      serve without trigger, approval, answer, or resume controls
--keep-workspace leave build workspaces on disk
--answer         answer an ask_user question in advance (repeatable)
--state          sqlite state database (default .steps/<pipeline>.db per YAML)
--name           name a pipeline inside the state db, e.g. --name infra=infra/pipeline.yml
--var / --vars-file   pipeline vars, as everywhere else
```
