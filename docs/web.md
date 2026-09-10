# The daemon

```bash
steps web                                        # starts empty
steps pipeline set -c pipeline.yml               # upload one into it
```

`steps web` is the long-running mode: it serves the browser UI at
`http://127.0.0.1:8088`, holds whatever pipelines have been set into it, polls
their `trigger: true` resources, and runs the jobs both of those enqueue.

**It takes no pipeline arguments.** A pipeline arrives by `steps pipeline set`
and by nothing else, which is what makes three things true that were not
before: vars belong to the pipeline you set rather than to the process,
a pipeline's identity is a name you chose rather than whatever its file was
called, and a change happens when somebody asks for it rather than when a file
moves. Nothing here watches a file.

Each served pipeline is routed under `/p/<name>/`, where the name is the one
`set` was given. All of them share one state database — `.steps/steps.db`
unless `--db` says otherwise — and stay strangers inside it; see
[One database, several pipelines](#one-database-several-pipelines).

## Setting a pipeline

```bash
steps pipeline set -p app -c app.yml -v repo_uri=https://github.com/acme/app
```

- **`-p` is the name on the daemon**, defaulting to the YAML's base name. It is
  the `/p/<name>/` route, the identity every recorded row is scoped by, and
  what every other verb takes.
- **`-c` is the file on YOUR machine.** It is read, `((var))`-substituted and
  parsed locally, then uploaded. The daemon never reads a path of yours.
- **`-v` / `--vars-file` are per-set.** Substitution happens before the upload,
  so the daemon holds the substituted text and one file set twice under two var
  sets is two pipelines. Vars are still not a secret store — see
  [templating.md](templating.md).
- **Includes travel with it.** A `run_file:`, `system_file:` or
  `message_files:` entry is pipeline-relative, and the daemon has no sibling
  filesystem, so `set` sends `{source, includes}` and the daemon resolves those
  paths against the upload and nothing else. An include it was not sent is a
  refusal, never a read of the daemon's own disk.
- **It diffs, then asks.** `set` fetches what the daemon is serving, prints
  what would change, and asks. `-n` skips the prompt and is what scripts pass.
- **It is compare-and-set.** The sha you diffed against is sent with the
  upload; if the configuration moved in between, the daemon refuses and says
  so, rather than applying yours over whatever arrived.

**The refusal lands where you asked.** That is the whole reason this is HTTP
rather than a row written into a database: `set` needs a synchronous answer
from the machine that will run the pipeline — is `api_key_env:` set *there*, is
the stdio MCP binary on *its* `PATH`, does `workspace.root:` exist. A
configuration that fails those is refused, the daemon goes on serving what it
had, and the terminal that asked prints why:

```
$ steps pipeline set -c app.yml
http://127.0.0.1:8088 refused it: app cannot run here:
  agent "reviewer"  $OPENROUTER_API_KEY is not set (source.api_key_env)
```

What it does NOT check is the network. A set that passes can still meet a
revoked key or a server that is down at run time; `steps validate --live` is
the command that asks.

## The rest of the family

```bash
steps pipeline list                      # what this daemon holds
steps pipeline get -p app                # the configuration it is serving
steps pipeline pause -p app              # stop polling, admitting and triggering
steps pipeline unpause -p app
steps pipeline rename -p app --to legacy # keeps the history
steps pipeline destroy -p app            # forgets it, and everything under it
```

Every verb takes `--target` (or `STEPS_TARGET`), defaulting to
`http://127.0.0.1:8088`. There is no `login` and no saved targets, because
there is nothing to log in to — see [Security](#security).

- **`get` is the only full copy** of a configuration once you have edited the
  file it came from. It prints the served source and every file it carried.
- **`pause` is the pipeline-level circuit breaker.** Paused, nothing is
  polled, nothing is admitted from the queue, a webhook delivery is received
  and enqueues nothing, and a manual trigger is refused with a message. Every
  page of that pipeline says so. It is a bigger switch than the per-job breaker
  `steps jobs` reports, and unrelated to it.
- **`rename` keeps history**, because every recorded row reaches its pipeline
  by row id rather than by name. That is new: while a pipeline's identity came
  from its filename, renaming was a different pipeline with empty state.
- **`destroy` is not recoverable.** The runs, the resource versions, the queue
  and the merkle cache go with the row. It asks unless given `-n`.

## What a restart does

Nothing. The daemon loads every pipeline's current configuration from its state
database at startup and serves it, so a restart picks up where the last process
left off — including on a machine where the YAML never existed. A daemon that
holds nothing serves an index saying how to set one.

## What it shows

| Route | Answers |
|---|---|
| `/` | With several pipelines served: what this process holds, and one run feed across all of them, newest first. With one, it redirects straight through |
| `/p/:pipeline` | Which jobs exist, how each last run went, and which jobs feed which — as a list, or as a dependency graph laid out from the `passed:` constraints, each node carrying its latest status |
| `…/runs` | One run history across every job of the pipeline, newest first — the cross-job view the per-job history can't give |
| `…/jobs/:job` | This job's dependencies in both directions, its run history with a duration trend, the resource versions it has passed against, and the resolved limits each agent step runs under |
| `…/runs/:run` | **The transcript**: every step in plan order, what it did, and — for agent steps — what the model said and which tools it called |
| `…/nodes/:hash` | What a merkle hash is made of, and every run that reused it: the cache's receipt |
| `…/config/:sha` | The pipeline as the runs pinned to that hash executed it — readable after the file on disk has moved on |
| `…/approvals` | Pending `approval:` steps, and the decisions already made |
| `…/questions` | Pending `ask_user` questions, and the answers already given |
| `…/resources` | Latest checked version per resource, and any job the circuit breaker has paused |
| `/docs` | These docs, rendered with syntax-highlighted examples — the same pages `steps docs` shows in a terminal |

Press `/` anywhere for a jump palette over pipelines, jobs, and recent runs — across **every** pipeline this process serves, not only the one whose page you are on. The one you are on ranks first, and a hit from anywhere else says which pipeline it belongs to.

### Agent dials

A job page lists the **resolved** limits of each agent step in its plan: turns, context ceiling, deadline, and spend budget, after the step, the agent and the built-in default have all had their say. It exists so "why did this step stop at 30 turns" is answerable without cross-referencing three files, and it shows `uncapped` rather than `0` for a dial an author explicitly removed — `0` in a limit column reads as the opposite of what it means. An `ensemble:` that decides with a judge (`decide: <agent>`) lists the judge as a row of its own, because it runs — and spends — as an agent step the plan never spells out.

`Turns` is one word for two units and the header says so: a hosted agent's turn is one request/tool-execute round driven by steps, while a CLI agent's is whatever the child reports as `num_turns` — one per tool round, pooled across every `messages:` entry — which runs far higher for the same work. A cap that looks generous beside a hosted source can truncate a CLI one mid-task.

The budget column carries one unit or the other, never both, because the two spellings are exclusive by source kind: a hosted agent is metered in `budget.tokens` and a CLI agent in `budget.usd` (see [attempts-timeout.md](attempts-timeout.md)). Read `uncapped` there together with the turn column: for a CLI agent `budget.usd` is the only ceiling anything enforces mid-conversation, so an uncapped budget beside uncapped turns means the step is held by its deadline and nothing else. The one exception is spelled out in the same cell: an `across:` block's own `budget:` caps what its cells spend *together* (see [control-flow.md](control-flow.md)), so such a step reads `uncapped per cell · 50,000 tokens for the matrix` rather than `uncapped`.

It covers the agents a *step* names. A task's `fix:` agent and a step's sub-agent `tools:` grants run under limits of their own and are not listed.

## The transcript

The run page is the point of the whole thing. It renders a run the way the
terminal does — steps in order, prefixed by kind, colored by outcome — with
the things a scrollback cannot give you:

- **Spend is shown against the ceiling it was spent under**, in an `of` column beside the cost, so a row reads `500,000` under `tokens` against `2,000,000 tokens` under `of` — or `$3.00` for a CLI agent, which is metered in dollars — which answers a question `500,000` alone does not. The ceiling is the agent's per-invocation one (an `across:` block's shared `budget:` is on the job page instead). It comes from the configuration currently loaded, so it is shown only for a run whose recorded config sha still matches it — a run started before an edit says `config changed` instead. That is deliberate and not a limitation to route around: a recorded revision stores its source but not its include files, so a run older than the last edit cannot be reconstructed, and "what was this capped at when it failed" is exactly the question a stale number would answer wrongly.

  The column distinguishes `uncapped` from `unknown`, and the difference is load-bearing. A ceiling belongs to the **agent**, while spend is recorded against the **step** — the same string for an ordinary step, and not for an `across:` cell, which renames itself. `uncapped` means the loaded configuration says that agent has no ceiling; `unknown` means the step's name resolved to no agent here. Reading a miss as `uncapped` would state the opposite of the truth for a step that had a ceiling, on the run where the ceiling is why it died.
- **A finish reason that outlived its step says so.** `finish` on the spend panel is the *provider's* word about the last request that completed — for a CLI agent, the last invocation's. A step whose first message finished cleanly and then ran out of a pooled `max_turns:` or `budget: usd` before the next message was asked records `success` on a step that failed, so that cell reads `success — step failed`. The provider's word is annotated, never overwritten: it is the only record of how the model itself stopped.
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
  passed run of that job — computed by comparing content hashes, so it names
  the steps whose inputs, command, or prompt actually moved.
- **Every run names the configuration it executed**, linking the pipeline as
  it was when that run started — which the file on disk no longer holds once
  anyone edits it. When a failed run's configuration differs from the last
  passed one's, the page says so above the step diff and links both, because
  "the steps moved because the pipeline did" and "the steps moved on their
  own" are different problems and the step diff alone cannot tell them apart.
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
  555 B`), so one 30KB tool result cannot bury the conversation around it. A
  string value that is not itself JSON is sniffed for a language — Go, YAML,
  a diff, a shebang script — and colored the same way, line by line.
- **What was written for a reader is rendered; what was written to the model,
  and what a program produced, is shown literally and colored.** A model's
  answer, and its running commentary mid-conversation, is markdown — headings,
  lists, tables, emphasis, because that is what a model writes for a person to
  read. A system prompt, a user message, a tool payload and a task's output are
  never re-rendered: they are text sent to a model or produced by a program,
  and showing them as anything other than what they are would show something
  that was never sent or printed. Each is still colored when the content can be
  told apart from prose with confidence — a diff's `@@` hunks, a YAML document,
  a Go file, an unfenced JSON blob — through the same highlighter `/docs` uses.
  Detection is best-effort over a fragment that may be truncated mid-token, and
  it declines rather than guesses: anything it cannot tell apart from prose
  renders as plain text. No detected language is ever named on the page — a
  guess is not a fact, unlike the byte counts and worker names beside it.

  Rendered markdown's renderer is a deliberately narrowed one, because the text
  came from a model. Raw HTML is dropped rather than parsed. A `javascript:` or
  `data:` link renders with no destination, and every surviving link carries
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
  what it was told. `steps runs where -p <pipeline>` reads the same rows in a
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

Five controls, each doing what a CLI verb does:

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
- **Abort** a running run from its page, or a queued one from the page a
  trigger lands on — what `steps runs abort` asks for. See
  [Aborting a run](#aborting-a-run).

`--read-only` withholds all five: the controls disappear from the pages and
the routes refuse. The queue is still drained, polling still runs, and
`steps pipeline set` still works — that flag is a statement about the
browser's surface, not about what the process does on its own or about how it
is deployed. `--listen 0.0.0.0:8088 --read-only` is a build box that still has
to notice new versions; read [Security](#security) before you expose one.

**The webhook route is the one exception, deliberately.**
`POST /p/<slug>/check/<resource>` still works under `--read-only`, and the job
it enqueues still runs. It is not a UI control: it carries the resource's own
token, which is a stronger check than the five above have, and withholding it
would mean a read-only box could not be the thing GitHub notifies — which is
most of why a build box is exposed at all. `--read-only` says a *browser*
cannot start work here; it does not say nothing can. If that is what you
want, do not give the pipeline a `webhook_token_env:` resource — with none, the
route is a 404. See [infra.md](infra.md#webhook-triggered-checks).

## Aborting a run

Stopping one run leaves the daemon and every other run alone:

```bash
steps runs abort -p app 46UMHVPYRA6YHB7M   # stop a running run
steps runs abort -p app --queued build     # drop build's queued run before it starts
```

or **■ Abort** on the run's page. It means what it means in Concourse:

- The run reads **aborted**, not failed — the status a Ctrl-C gives a
  `steps run` too. Its `on_abort:` and `ensure:` hooks still run, with the
  60-second grace every abort gets, so the command returns before the run has
  finished stopping.
- Nothing it did is cached as done: running the job again runs the aborted
  step again, and no version is marked as having passed it.
- A placed step is interrupted on its worker rather than orphaned there, and a
  machine the run acquired is given back the way any finished run gives it
  back.
- Its queue row is finalized **aborted**, so a restart does not run it again,
  the circuit breaker neither counts nor clears it, and its `serial:` /
  `max_in_flight` slot frees once the run has actually stopped.
- An aborted queued run keeps its row, reads aborted, and never starts.
  `--queued` names a job rather than a run because a queued build has no run
  id yet, and a job has at most one queued at a time.

It goes through the daemon rather than the database — what it stops is a
context inside that process — so it needs a running `steps web`, takes
`--target` like `steps pipeline` does, and is refused under `--read-only`. A
run this daemon is not executing — one a `steps run` started against the same
file, or one a crashed daemon left marked running — is refused with a message
saying so.

## One daemon

`steps web` is the whole long-running mode: it serves the UI, holds the
pipelines, polls every `trigger: true` resource, and drains the queue all of
that fills. There is no separate watcher and no one-shot — a front end that
drains a queue nothing fills is a runner that looks alive and notices nothing,
and two processes against one state database claim each other's work.

```bash
steps web                      # serve, and poll every 30s
steps web --interval 5m        # slower
steps web --max-concurrent 4   # up to four queued jobs at a time
```

**There is no `--once`.** It was the cron form of a runner: load a file, poll
once, exit, never bind. A process that never binds has nothing to be set into,
and a server is its own scheduler — run it under systemd as a service rather
than a timer, and let `--interval` be the schedule.

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
  the UI is where you would run them from.
- **Preflight runs before the first poll**, the same check `steps web` does
  and with the same asymmetry: a problem *waiting cannot fix* — an `mcp:` tool
  the server does not expose — stops that pipeline's polling and says so,
  while a problem waiting might fix — a server that did not answer, a token a
  refresh would renew — is printed once as `(transient — polling anyway)` and
  left to the loop, which retries by its nature. `--no-preflight` skips the
  check entirely. It runs inside the poller, so it never delays serving.

## Applying a change

Set it again. There is no watcher and no `--watch`:

```bash
steps pipeline set -c pipeline.yml       # edit, set, and it is serving
```

**A save-to-apply loop is a one-line wrapper** around this — `watchexec -w
pipeline.yml -- steps pipeline set -c pipeline.yml -n` — and that is where it
belongs. A watcher inside the daemon is an implicit set that fires with nobody
attached, which is exactly why it needed a held-configuration banner nobody was
looking at: deleting it deletes the banner, the hold state, and the question of
what a daemon does with a configuration it will not accept. It refuses it, to
your face.

**A run in flight finishes against the configuration it started under.** The
swap is immediate for everything after it: the pages, the trigger poller, the
webhook endpoints, and the next job the queue admits — including the `serial:`,
`serial_groups:` and `max_in_flight:` a job is admitted under, which live in the
database and are rewritten from the new configuration on every set. What the
running job is executing does not change underneath it, and the run records
which configuration that was — the `CONFIG` column `steps runs` prints and the
revision named on the run page, where it links the configuration itself. A
`--resume` is the exception, and deliberately: it continues a failed run under
the configuration it is resumed *with*, which is usually the one that fixed it.

**Adding or removing a `trigger: true` get takes effect too.** The poll loop
re-decides what it can check each time the configuration changes, so a resource
a set adds is preflighted and then polled, and one a set removes stops being
checked. A pipeline with nothing to poll is a state the loop sits in, not a
reason it was never started.

**A changed `workspace:` is adopted.** The provider that materializes build
directories is rebuilt when the block moves, and a run already in flight keeps
the one it started with until it finishes — so a set never deletes the tree a
build is working in. One this machine cannot provide is refused like any other
part of the configuration.

## One database, several pipelines

A daemon holds every pipeline set into it in ONE state database — `.steps/steps.db`
unless `--db` names another:

```bash
steps web --db /var/lib/steps/state.db
steps pipeline set -p app -c app.yml
steps pipeline set -p infra -c infra/pipeline.yml
```

A bare path is a sqlite file, and so is `sqlite:///var/lib/steps/state.db`
— the scheme is how a second driver will be chosen, the way `--worker` takes
`ssh://` and `aws://`, and sqlite is the only one today. A scheme no driver
answers to is refused before anything is opened.

One file to back up, and one file to delete. What it is *not* is a merge:
inside the database every row carries the pipeline it belongs to, so histories,
resource versions, queues, serial groups and the merkle cache stay separate.
Two pipelines each with a job named `build` running an identical task do not
share a cache entry, and one pipeline's `run_history:` cap never reaps
another's runs.

Reading it back needs no pipeline argument. `steps runs` lists what the file
holds and interleaves the newest runs of all of it, which is the terminal's
version of the web root:

```bash
$ steps runs --db /var/lib/steps/state.db

PIPELINE  PATH
app       /src/app/app.yml
infra     /src/infra/pipeline.yml

WHEN                 PIPELINE  JOB      STATUS     RUN
2026-08-30 09:14:02  infra     deploy   succeeded  UNVFHMCHVWY6GV6N
2026-08-30 09:12:40  app       build    failed     46UMHVPYRA6YHB7M
```

`PATH` is where the configuration was last set FROM — a file on whoever's
machine ran `set`, recorded so a reader can tell two checkouts apart, and never
opened here.

**The read commands take `-p <name>`, not a path**, because a served pipeline
has no file on this machine:

```bash
steps runs -p app                      # what ran
steps runs steps -p app                # why a step did what it did
steps runs cost -p app 46UMHVPYRA6YHB7M
steps approvals -p app
steps questions -p app
steps jobs -p app
```

They read the database directly rather than going through the daemon, so they
work against a stopped one — and they take `--db` when it is not the default.
`steps run`, `steps test`, `steps validate` and `steps plan` still take a file
path and still default to `.steps/<filename>.db` beside it: those are the local
commands, and a local command has a file.

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

**`steps pipeline set` is a remote-shell endpoint. Say that plainly: a
pipeline is arbitrary commands, so anyone who can reach this port can run
anything they like as the user running the daemon.** No token, no password, no
allow-list. `--read-only` does not close it either — that flag has always been
a statement about the *browser's* surface, and the set endpoint is the
deployment path, not a button on a page.

That is the reason for the loopback default, and it is a stronger reason than
the trigger controls ever were. **Binding to a routable address hands the
machine to whoever can reach the port.** `--listen 0.0.0.0:8088` exists for
someone who has decided that is what they want; put it behind something that
authenticates — an SSH tunnel, a reverse proxy, a network nobody else is on —
and treat `--read-only` as being about the browser only.

**Loopback keeps other machines off the port, not other web pages.** A page
open in a browser on this machine can re-point its own hostname at `127.0.0.1`
after it loads — DNS rebinding — and from then on its requests reach this port
carrying that name as both `Host` and `Origin`, which is exactly what a
same-origin request looks like. So everything under `/api/` — what
`steps pipeline` and `steps runs abort` talk to — refuses a request carrying a
header only a browser attaches: an `Origin` of any value, which every browser
sends on the `PUT`, `POST` and `DELETE` those verbs use, or a `Sec-Fetch-Site`
saying anything but `none`, which is a URL typed into the address bar. The CLI
sends neither, and no page in the UI calls `/api/`. `Host` is not checked,
because a reverse proxy in front of the daemon forwards the name the client
used.

The browser's own controls — trigger, approve, answer, resume, abort — are
`POST`s refused when their `Origin` names another host, which stops another
site aiming a form at your port. A rebinding page is not another host: it can
read every page, configurations included, and press those controls, unless
`--read-only` has withheld them. It cannot set, destroy, rename or pause a
pipeline.

## Flags

`steps web` — facts about the box, all of them:

```
--listen         address to serve on (default 127.0.0.1:8088)
--interval       how often to poll trigger: true resources (default 30s)
--max-concurrent maximum queued jobs running at once, per pipeline (default 1)
--pin / --force  pin a version field; ignore the cache and re-run every step
--no-preflight   skip the pre-poll health check of models and MCP servers
--read-only      serve without trigger, approval, answer, resume, or abort controls
                 (steps pipeline set is NOT withheld — see Security)
--keep-workspace leave build workspaces on disk
--answer         answer an ask_user question in advance (repeatable)
--worker         map a step tag to a machine, e.g. --worker gpu=ssh://jt@box
--db             state database: a sqlite path or sqlite:// url (default .steps/steps.db)
```

`steps pipeline <verb>` — facts about one pipeline:

```
--target         the daemon to talk to (default http://127.0.0.1:8088, $STEPS_TARGET)
-p / --pipeline  its name on the daemon (set defaults to the YAML's base name)
-c / --config    the YAML to upload (set only)
-v / --var, --vars-file   pipeline vars, substituted before the upload (set only)
-n               do not diff or ask; apply it (set, destroy)
--to             the new name (rename only)
```
