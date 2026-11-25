# The daemon

```bash
steps web                                        # starts empty
steps pipeline set -c pipeline.yml               # upload one into it
```

`steps web` is the long-running mode: it serves the browser UI at
`http://127.0.0.1:8088`, holds whatever pipelines have been set into it, polls
their `trigger: true` resources, and runs the jobs both of those enqueue.

**It takes no pipeline arguments.** A pipeline arrives by `steps pipeline set`
and by nothing else, so vars belong to the pipeline you set rather than to the
process, a pipeline's identity is a name you chose rather than whatever its
file was called, and a change happens when somebody asks for it rather than
when a file moves. Nothing here watches a file.

Each served pipeline is routed under `/p/<name>/`, where the name is the one
`set` was given. All of them share one state database — `.steps/steps.db`
unless `--db` says otherwise — and stay strangers inside it; see
[One database, several pipelines](#one-database-several-pipelines).

**Its terminal is log lines, nothing else.** A run is read on its page, so
what a step prints, its notes and its prompts never reach the daemon's
stdout. Stderr carries the log: where it serves, what polling enqueued, each
job starting and finishing, workers and failures. `--log-level debug` adds
each step starting, skipping and noting.

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
  [templating.md](templating.md). `--var` values are substituted into the pipeline text, uploaded to the daemon, stored in the revision and shown in confirm diffs and `pipeline get`; keep secrets in `secret:` sources (see [secrets.md](secrets.md)), not `--var`.
- **Includes travel with it.** A `run_file:`, `system_file:` or
  `message_files:` entry is pipeline-relative, and the daemon has no sibling
  filesystem, so `set` sends `{source, includes}` and the daemon resolves those
  paths against the upload and nothing else. An include it was not sent is a
  refusal, never a read of the daemon's own disk.
- **It diffs, then asks.** `set` fetches what the daemon is serving, prints
  what would change, and asks. `-n` skips the prompt and is what scripts pass.
- **`--pause` sets it paused.** The pause is written with the configuration,
  before the pipeline starts, so a new pipeline never gets its first poll. A
  `set` followed by a separate `pause` leaves a gap where that poll, and any
  build it starts, can slip in.
- **It is compare-and-set.** The sha you diffed against is sent with the
  upload; if the configuration moved in between, the daemon refuses and says
  so, rather than applying yours over whatever arrived.

**The refusal lands where you asked.** That is the whole reason this is HTTP
rather than a row written into a database: `set` needs a synchronous answer
from the machine that will run the pipeline — is `api_key_env:` (or a resource expression's `env("NAME")`) set *there*, is
the stdio MCP binary on *its* `PATH`, can its `--workspace-root` be written. A
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
steps pipeline rename -p app --to legacy # keeps the history and its mcp logins
steps pipeline destroy -p app            # forgets it, everything under it, and its mcp logins
```

Every verb takes `--target` (or `STEPS_TARGET`), defaulting to
`http://127.0.0.1:8088`. There is no `login` and no saved targets: on a daemon
with accounts, the credentials ride in the target URL's userinfo
(`http://user:password@host`) — see [authentication.md](authentication.md#from-the-cli).

- **`get` is the only full copy** of a configuration once you have edited the
  file it came from. It prints the served source and every file it carried.
- **`pause` is the pipeline-level circuit breaker.** Paused, nothing is
  polled, nothing is admitted from the queue, a webhook delivery is recorded
  and builds on unpause, and a manual trigger is refused with a message. Every
  page of that pipeline says so, and offers to release it. It is a bigger
  switch than the per-job breaker `steps jobs` reports, and unrelated to it.
- **`rename` keeps history**, because every recorded row reaches its pipeline
  by row id rather than by name. It is **refused while a build of the pipeline is running**, naming the builds
  — the served pipeline is rebuilt under the new name, which would cancel
  them — so wait for them, or stop them with `steps runs abort`. Queued
  builds are not a reason to refuse: they are kept, and run under the new
  name.
- **`destroy` is not recoverable.** The runs, the resource versions, the queue
  and the merkle cache go with the row. It asks unless given `-n`.

## What a restart does

Nothing you have to redo. The daemon loads every pipeline's current
configuration from its state database at startup and serves it, so a restart
picks up where the last process left off — including on a machine where the
YAML never existed. Queued runs are kept, and a build the restart interrupted
is [resumed](workspace.md#a-daemon-restart-resumes-the-build-it-interrupted),
skipping the steps it finished. A daemon that holds nothing serves an index
saying how to set one.

A pipeline the restart **cannot** serve — a variable it needs is unset in the
daemon's environment, an MCP binary it names is gone — does not
stop the others. Every pipeline that restores is served, and each one that did
not is listed as **broken**, with the reason, on `/`, in `/api/pipelines` and
in `steps pipeline list` (and printed as the daemon starts). Nothing about it
is served: its pages and its API verbs answer 404 naming the reason, and a
webhook delivery to it a bare 404, since a sender never passed the daemon's
credentials. `steps pipeline set -p <name>` with the same configuration
repairs it once the cause is fixed, and says `repaired:`; a missing variable is
the daemon's own environment, so that one needs a restart with it set, or a
configuration that does not need it. `steps pipeline destroy` removes a broken
pipeline whose cause cannot be fixed, and a rename onto or from one is refused.
Only a state file the daemon cannot read at all stops it from starting.

## What it shows

| Route | Answers |
|---|---|
| `/` | With several pipelines served, or any held [broken](#what-a-restart-does): what this process holds, one quiet row each, in name order — its mark, one named cell per job, its latest run, and what moves it next. A cell is its job's name on a tint of its status color, taken from the job's latest *finished* run like the [tab mark](#the-tab-mark), with a yellow bar along its bottom while a run is in flight and `⊘` while the breaker holds it; a dashed cell never ran. Only a cell that makes the row red (failed, errored, or `⊘` held) draws a glyph too, so a healthy row's mark is its only one. Under each cell are ten slots of the job's runs, newest at the right, an errored one hollow and an unfilled one a faint floor line; a pipeline of more than four jobs keeps that strip only on its red ones. Past six jobs the cells would wrap a row to a screenful, so the row is a **job map** instead: one tick per job in config order, in the strips' shapes (failed taller, running half filled, errored hollow, never ran a floor line, held tall and hollow), a named cell only for the jobs that are failed, errored, held or running — three, then `+N` — and a tally in words (`24 jobs · 2 failed · 4 never ran`) linking to the pipeline; `h`/`l` walk on into the ticks. An arrow joins a job to the one whose `passed:` it takes. Every cell is one click from that job's transcript. Latest names the job and is the newest run that made the row red when one did, otherwise the newest run; a running one counts up, a finished one is one unit of age. A passed Latest is only its job and age, dim; any other says its status in color, with no glyph. Next is how long until a cron's next slot (`in 5h`, the clock time on hover), else the trigger resources the pipeline waits on from outside, the first named and the rest counted (`on tagged +2`; a `passed:` hand-off is not one, and a held job's triggers are not counted), else *manual*; blank while paused, which the mark already says. A red row — a job failed or errored, the breaker holds one, or the pipeline is broken — carries a red stripe, and every other row's Latest is dim. A row says more only when something is not normal: what waits on you, what its queue still owes, or paused. Its pause button sits at the end of the row, shown when you hover or tab into it. Below is one run feed across every pipeline, newest first: the latest ten, or fifty with `?runs=50` (the **show 50 runs** link). A broken pipeline is a row with its reason and no links. With one served and none broken, it redirects straight through |
| `/p/:pipeline` | [The pipeline graph](#the-pipeline-graph): which jobs exist, how each last run went and how long ago, and what feeds each one. Hover a job for how long its run took, why the breaker holds it, or what a queued run waits on |
| `…/runs` | One run history across every job of the pipeline, newest first, 50 to a page with an `older` link. `?job=` narrows it to one job (the job row above the table, or any job name in it); `?q=` keeps runs whose id, status, starter or input versions hold the words, so a Honeybadger fault id, a PR number or a sha finds the run that handled it. Both are in the URL, so a narrowed history can be sent to someone. A finished run has the job page's ⟲ Retry beside it |
| `…/jobs/:job` | Where the job stands, without another click: it forwards to the job's **latest run**, running included; with no run but a trigger queued, to the [follow page](#following-a-run-you-started) for that trigger; with neither, to the detail page. Every link that names a job goes here, so a job is one click from its transcript from anywhere. It is a temporary redirect on purpose — a bookmark to it re-resolves on every visit |
| `…/jobs/:job/detail` | This job's dependencies in both directions, its run history with a duration trend, the resource versions it has passed against, and the resolved limits each agent step runs under. The one job link that leads here rather than to a run is the breadcrumb on a run, a node or the follow page — from inside a run, "latest" is already on the page |
| `…/jobs/:job/follow` | The [waiting page](#following-a-run-you-started) a trigger lands on, until the queued run starts |
| `…/runs/:run` | **The transcript**: every step in plan order, what it did, and — for agent steps — what the model said and which tools it called. A run that died on an mcp server links the server it blames — or, when the server answers but lacks a tool an agent grants, says so rather than asking whether it is connected. A run preflight refused is here too: errored, no steps, the reason in its head |
| `…/nodes/:hash` | What a merkle hash is made of, and every run that reused it: the cache's receipt |
| `…/config/:sha` | The pipeline as the runs pinned to that hash executed it — readable after the file on disk has moved on. Lines are numbered, and each named entry is a place to land: `#job-<name>`, `#resource-<name>`, `#resourcetype-<name>`, `#agent-<name>`, `#task-<name>`, `#mcp-server-<name>`, `#mcp-export-<name>` scroll to that entry and mark it, and a step is a place of its own inside its job, addressed by the keys that reach it (`#job-build:plan.2`, `#job-build:plan.2.do.0`, `#job-build:plan.1.on_failure`). A name the file declares elsewhere is a link to its entry (`get: weekly`, a resource's `type:`, `agent:`, `task:`, a `passed:` job, a tool's `mcp:`), a `run_file:` or `system_file:` path links the file's contents below, and the `name:` of a job, a resource or an MCP export links its page. A run's page links its job in the configuration the run executed, and each of its steps links that step (a nested step it cannot tell from a sibling links the step around it); a job's, a resource's, an MCP server's and an export's page link theirs in the configuration being served, and each run in a job's history links the configuration it executed, marked when that is not the current one |
| `…/approvals` | Pending `approval:` steps, oldest first, then a page at a time of the decisions already made, each naming the run it held |
| `…/questions` | Pending `ask_user` questions, oldest first, then a page at a time of the answers already given |
| `…/resources` | Each resource's status (checked, failing, never checked), latest version, and the jobs that use it, with why any is failing its check. **Check** checks one now rather than at the next poll — any resource, a `trigger: true` one or not, as Concourse's check button does; a paused pipeline refuses it |
| `…/resources/:resource` | Every recorded version, newest first, with **Check** and a ▶ per job that runs that job pinned to that version (an operator only to a version that passed the job's `passed:`), and a column per job that gets the resource: the latest run of that version and how many there were, or why there is none — **queued** (the job has a queue row and will build it), **waiting** (it would, but nothing has queued the job), **superseded** (a latest-mode job only builds the newest version), **not pinned** (the job only builds its `version:` pin), or **reaped** (a `version: every` job took it, and [`run_history:`](infra.md#how-much-history-to-keep-run_history) has since removed the run) |
| `…/mcp` | Every `mcp_servers:` entry, who depends on it (linked to the jobs an agent runs in), and whether it is wired up — with **Connect** to finish an oauth login in this browser and **Test** to probe one server. Present only for a pipeline that declares servers; see [mcp.md](mcp.md#authorizing-from-the-browser-the-mcp-tab) |
| `…/mcp/exports/:export` | One `mcp_exports:` entry: its address, its tools, its recent calls — see [mcp.md](mcp.md#where-an-export-is-seen) |
| `/activity` | [What people did](#activity), across every pipeline. Admins only |

Every list among them — runs, resources, mcp, approvals, questions — narrows the same way: a search box, a row of chips per field (job, status, type, transport, auth, as the page has them), and above the table the narrowings in force, each with **×** to drop it. A narrowed list is its URL, so it can be sent to somebody, and says how many rows it kept — "2 of 7" where it knows the whole list, "50 shown" where it is paged. Runs, approvals and questions page back with **older**. On a phone a row of a list becomes a small card — its status, name and numbers on a wrapping line, long sentences and versions on a line of their own, its button at the end — so nothing to press is behind a sideways scroll.
| `/connections` | The agents you have let connect to an export, with **Revoke**; on a daemon people sign in to — see [mcp.md](mcp.md) |
| `/login` | The sign-in form, on a daemon with accounts — see [authentication.md](authentication.md#signing-in-from-a-browser) |
| `/healthz` | `ok` and nothing else, for a platform's health check; needs no credentials |
| `/docs` | These docs, rendered with syntax-highlighted examples — the same pages `steps docs` shows in a terminal |
| `/mark`, `…/mark`, `…/runs/:run/mark` | Not a page: the tab mark alone, for every pipeline, one pipeline, or one run. A tab in the background stops refreshing its page and asks this every 30 seconds instead, so a pinned tab's icon and title stay current — within about a minute, since browsers slow a background tab's timers |

### The pipeline graph

The graph is drawn the way Concourse draws a pipeline. Resources are boxes: what a
job gets sits on its left and what it puts sits on its right, including a put
in a hook. A solid line means the get triggers the job. A dashed line means the
job only reads it. A `passed:` constraint arrives from the upstream job's
output, or through a box of its own when that job only gets the resource. A
job is as tall as the resources landing on it, so each line meets it at its
own point.

A put in one job and a get of the same resource in another are **not**
joined, because nothing makes the second job wait for the first. A line on
this graph promises that ordering, and only `passed:` makes it. So a resource
read beside two jobs in one column is one box, while the same resource further
right gets its own box. A line that would cross a column runs through a dashed
copy of its resource instead.

### Finding your way

The header is a path: **steps / *pipeline* / *section***. The pipeline comes
first because every tab after it — jobs, runs, resources, approvals,
questions, and mcp when the pipeline declares servers — is about that one
pipeline; click its name to switch to another the daemon holds. The root,
the docs and an error page sit above any pipeline, so they offer only the
switcher and no tabs. On a phone, upright or held sideways, the header is one row: the pipeline, a ☰
menu named for the section you are on, and **jump**. The menu holds the tabs,
read-only, the theme and your account, and shows the total of every tab's
count, so something waiting on you is still visible with the menu closed. A
paused pipeline leaves a blue line under the bar. **jump** (or `/`) opens a
palette that searches jobs, runs and pipelines across everything served.
Beside it is the theme: **auto** follows the system's
light or dark setting, and a press pins **light**, then **dark**, then back
to auto. The choice is this browser's own, kept in its local storage; the
daemon stores nothing.

On the root, `j`/`k` (or the up and down arrows, once inside the table) move
between pipelines, `h`/`l` (or left and right) move along a row's job cells,
and Enter opens what is focused. The cells are not Tab stops, so Tab passes
one pipeline at a time. Where you are survives the page refreshing itself.
A screen reader hears a pipeline once, when it turns red, and the same line
again when the keys land on its name; a page opened on a red fleet, a
recovery, and runs starting stay quiet.

The footer carries the docs, the source and the version this daemon was built
from — the thing to quote in a bug report.

### What needs you

The header counts work that is **stuck and waiting on a person**, on the tab
that fixes it, and spells each one out in a list under the header. It is
absent when nothing is waiting — the presence of the list is the signal, so it
has no resting state.

| Shown as | Means | Fixed by |
|---|---|---|
| `jobs ●N` | N jobs the circuit breaker is holding after repeated failures | **Release** on the job's page |
| `resources ●N` | N resources whose `check` is erroring. A poll stops at the first one, so ONE broken check stops the pipeline triggering | the resources page, which shows what the check said |
| `mcp ●N` | N declared `mcp_servers:` that cannot be used — a login expired, a command missing, a variable unset | **Connect**, or the thing the status names |
| `approvals ●N` | N `approval:` steps parked on a decision | approving or rejecting |
| `questions ●N` | N `ask_user` questions parked on an answer | answering |

The pipeline switcher carries the same count for every pipeline this daemon
holds, and the root lists it per pipeline — otherwise a daemon holding
several hides every one but the open tab. Each switcher row also leads with
that pipeline's [mark](#the-tab-mark) — `✗` for a failed job, `⏸` paused —
circled in yellow while something runs, or `◐` alone when nothing has
finished yet, so a broken pipeline is found by scanning one column rather
than opening each. A red pipeline being rebuilt is one circled `✗`, never
`✗` and `◐` side by side.

Activity is shown differently from attention, because a signal that is never
quiet is one nobody reads. Runs in flight appear as a dim `◐N` beside a
switcher row — quiet the moment nothing is running, and never in the yellow
of what waits on you. Failures appear as a pipeline's mark and never as a
count: a count that only clears when somebody fixes the world would be
permanent. How many resources exist does not appear at all.

Press `/` anywhere for a jump palette over pipelines, jobs, and recent runs — across **every** pipeline this process serves, not only the one whose page you are on. The one you are on ranks first, and a hit from anywhere else says which pipeline it belongs to.

### Agent dials

A job page lists the **resolved** limits of each agent step in its plan: turns, context ceiling, deadline, and spend budget, after the step, the agent and the built-in default have all had their say. It exists so "why did this step stop at 30 turns" is answerable without cross-referencing three files, and it shows `uncapped` rather than `0` for a dial an author explicitly removed — `0` in a limit column reads as the opposite of what it means. An `ensemble:` that decides with a judge (`decide: <agent>`) lists the judge as a row of its own, because it runs — and spends — as an agent step the plan never spells out.

`Turns` is one word for two units and the header says so: a hosted agent's turn is one request/tool-execute round driven by steps, while a CLI agent's is whatever the child reports as `num_turns` — one per tool round, pooled across every `messages:` entry — which runs far higher for the same work. A cap that looks generous beside a hosted source can truncate a CLI one mid-task.

The budget column carries one unit or the other, never both, because the two spellings are exclusive by source kind: a hosted agent is metered in `budget.tokens` and a CLI agent in `budget.usd` (see [attempts-timeout.md](attempts-timeout.md)). Read `uncapped` there together with the turn column: for a CLI agent `budget.usd` is the only ceiling anything enforces mid-conversation, so an uncapped budget beside uncapped turns means the step is held by its deadline and nothing else. The one exception is spelled out in the same cell: an `across:` block's own `budget:` caps what its cells spend *together* (see [control-flow.md](control-flow.md)), so such a step reads `uncapped per cell · 50,000 tokens for the matrix` rather than `uncapped`.

It covers the agents a *step* names. A task's `fix:` agent and a step's sub-agent `tools:` grants run under limits of their own and are not listed.

## The transcript

The run page is the point of the whole thing. It renders a run the way the
terminal does — steps in order, prefixed by kind, colored by outcome — with
the things a scrollback cannot give you. Its head is two lines: what happened
and where (status, the step that broke, duration, when), then what it ran
against and what it cost (configuration, workspace, neighbours, the run's
total spend). The steps follow within the first screen, and each step carries
what it spent and where it ran on its own row.

- **Spend is shown against the ceiling it was spent under**, on the agent step's own row: its header names the model and totals the tokens, cache hits and cost, and its body breaks them down and ends `of <ceiling>`, so it reads `$0.42 of 2,000,000 tokens` — or `$3.00` for a CLI agent, which is metered in dollars — which answers a question `500,000` alone does not. The ceiling is the agent's per-invocation one (an `across:` block's shared `budget:` is on the job page instead). It comes from the configuration currently loaded, so it is shown only for a run whose recorded config sha still matches it — a run started before an edit says `config changed` instead. That is deliberate and not a limitation to route around: a recorded revision stores its source but not its include files, so a run older than the last edit cannot be reconstructed, and "what was this capped at when it failed" is exactly the question a stale number would answer wrongly.

  The ceiling distinguishes `uncapped` from `unknown`, and the difference is load-bearing. A ceiling belongs to the **agent**, while spend is recorded against the **step** — the same string for an ordinary step, and not for an `across:` cell, which renames itself. `uncapped` means the loaded configuration says that agent has no ceiling; `unknown` means the step's name resolved to no agent here. Reading a miss as `uncapped` would state the opposite of the truth for a step that had a ceiling, on the run where the ceiling is why it died.
- **A finish reason that outlived its step says so.** `finish` on an agent step is the *provider's* word about the last request that completed — for a CLI agent, the last invocation's. A step whose first message finished cleanly and then ran out of a pooled `max_turns:` or `budget: usd` before the next message was asked records `success` on a step that failed, so it reads `success — step failed`. The provider's word is annotated, never overwritten: it is the only record of how the model itself stopped.
- **A step that stopped early says so.** An agent whose turn budget ran out is asked to answer from what it already gathered, and the answer is *degraded* — afterwards it is indistinguishable from a confident one unless the record says otherwise. It carries a `stopped early` badge, live and on reload alike. A `truncated` badge answers the other half: a response cut off mid-sentence by the model's own output limit.
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
- **Only the path to a failure or to live work opens by default.** A reader
  opening a finished run came for what broke, so a failed, errored or
  aborted step opens, and so does every block holding one; while a run is
  live, so does a running block and the agent whose conversation is
  streaming in. A put opens
  on the version it produced, and a step the run never heard the end of opens
  too. Everything else — a passed agent's transcript, a passed block, a
  failure a `try:` tolerated, a sibling `fail_fast:` aborted — is one click
  or <kbd>e</kbd> away. A row you open, close or focus stays the way you left
  it, including across the reload when the run ends.
- **The rail lights along the branch that is working.** While a run is in
  flight, every block holding something still running is marked, so a reader
  who has folded half the page still knows where to look. A failed branch is
  not marked the same way — the counts above already carry the failure up every
  ancestor.
- **The job's other runs are one click away, on the page.** A strip under
  the header lists this job's recent runs, newest first, as status-colored
  chips — the one you are on is marked, a trigger that has not become a run
  yet leads with a `queued` chip to its follow page, and `all runs` opens the
  run history narrowed to this job. It refreshes on its own, so an older run of a job says
  when a newer one starts.
- **Keyboard**: <kbd>j</kbd>/<kbd>k</kbd> walk the tree, <kbd>e</kbd>/<kbd>c</kbd>
  expand and collapse everything, <kbd>enter</kbd> toggles, and <kbd>f</kbd>
  jumps to the innermost failure — the step that actually broke, rather than
  the outermost block that reports it.
- **A cached step is folded and dimmed**, labeled `unchanged — replayed from
  cache`. The steps a chain-skip swallowed are shown too, for the same reason:
  a transcript that stops at the cache hit reads as a truncated run rather
  than a cheap one.
- **A failed run leads with where it broke**: its header names the innermost
  failing step — the one that actually broke, which <kbd>f</kbd> also jumps
  to — as a link, and the error is read on that step, once. The head carries
  an error only when no step holds it: a run that died on an image pull, a
  placement or a resource check before any step failed. The page also says
  what changed since the last passed run of that job — computed by comparing
  content hashes, so each step whose inputs, command, or prompt actually moved
  carries a `changed` mark on its own row (`new` for a step that run did not
  have), and the line under the header counts them. A step that broke
  because its content moved carries `changed`; one that failed with the same
  content as before does not; hooks are never compared.
- **Every run says who or what started it**, after when it started: a
  person by name and where the name came from (`by ops (local)`), or what
  stood in for one (`by new version of repo`, `by webhook: push`,
  `by upstream jobs passed`, `by more versions to build`). A retry says what it repeated first
  (`rerun of #a1b2c3 by ops (local)`), and a restart keeps the run's original
  starter. A run started from a terminal with `steps run` names whoever ran
  the command, marked `(this machine, unverified)`: nothing checked that
  name, and the page says so in words. A run recorded before steps kept this
  names nobody rather than guessing.
- **Every run names the configuration it executed**, linking the pipeline as
  it was when that run started — which the file on disk no longer holds once
  anyone edits it. When a failed run's configuration differs from the last
  passed one's, the page says so above the step diff and links both, because
  "the steps moved because the pipeline did" and "the steps moved on their
  own" are different problems and the step diff alone cannot tell them apart.
- **A put opens on the version it produced**, `put: image (version: {...})`,
  as a get's row opens on the version it fetched — or says `(no version)`
  when its `out:` printed none. The note that the put made the rest of the
  chain uncacheable sits on the same row.
- **A task step expands into what it printed.** Output is captured as it
  prints and bounded at 32KB per step (see [Live runs](#live-runs)). Recorded
  whichever way the step ended, and especially when it failed: the error a
  task returns names the exit status, and an assert mismatch names the
  expectation — neither carries the output that explains either. A step that
  printed nothing is not expandable at all, so a chevron never promises detail
  that is not there.
- **An output the record cut keeps its middle, in the state database.** When a step
  prints past the 32KB record, the whole of it is stored as it prints, and the
  record's line says `full output kept`. The step's row then links `full
  output`, served as plain text at
  `/p/<pipeline>/runs/<run>/steps/<step>/output/<n>` behind the same
  sign-in, role and `access:` checks as the run page: a viewer who may not see
  the run gets the answer the run page gives them. What is stored is written
  after the secret scrubber, so a value masked in the record is `***` there
  too, and it stops at 64MiB with a line saying how much more was dropped. A
  step that fits in the record stores nothing. `steps runs output <run> <step>
  -p <pipeline>` prints the same whole output in a terminal, and the record
  itself for a step that stored nothing. Leaving the step off — `steps runs
  output <run> -p <pipeline>` — lists the steps that recorded output
  instead, one per line, in the order each first did — a name holding a
  control character or line break prints Go-quoted (`"two\nlines"`), and
  that quoted line is accepted back as `<step>`; a step still printing
  joins the list when it finishes, the list naming only what this command can
  print. How, and for how long, is under [Live runs](#live-runs).
- **An agent step expands into its conversation**: the model's text each turn,
  every tool call, and any sub-agent delegation nested underneath, closing with
  the step's `answer`. Each model reply carries how long the request behind it
  took, measured from when that request was sent (hover it for both times, in
  UTC); a reply that took more than a tenth of the step's `timeout:` is flagged,
  so the slow ones stand out on a folded page. A step that failed while waiting
  on the model says how long that last request had been in flight — for a
  timed-out step, usually the answer to where the time went. Its system and
  opening prompts are the same on every run of a job, so each is one line —
  its first words and size — that opens in place.
- **Which image a step ran in is named whole once, in its body**: the step's
  placement line starts with the `get` it came from and the whole reference
  (`image toolchain → alpine@sha256:…`), then the machine. Its row only points
  at it: a tag as written (`golang:1.25`), an image pinned by digest cut to its
  first twelve characters (`[af8016cedcbf]`), never cut shorter at any width.
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
- **Which machine a step ran on**, on that step's own row, for a step that was
  placed: the tag, the platform the worker reported,
  how many bytes had to be pushed to it and how many came back, the identity
  it ran as, and the machine — plus the image if the step ran in a container
  on it, with the digest it resolved to there (shortened; the row's head names
  the image, and hovering it shows the digest). A worker that named no identity — every
  docker+ worker, whose step runs as its image's user — leaves it out rather
  than inventing `0:0`, which would read as root.

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

## Activity

`/activity` — an admin's page, reached from the menu under your name on a
server you sign in to — is the log of what people did to the daemon's
pipelines, newest first, across every pipeline: who set one, triggered or
retried a job, approved or rejected, answered a question, aborted, paused,
released, renamed, destroyed, or connected, tested or disconnected an MCP
server. Each line says when, who,
what, and to which pipeline, and the job, run, revision or page it touched
is a link.

- **Every name narrows it.** Click a person, an action or a pipeline and the
  log shows only those; what it is narrowed by is said above the table, each
  with an `×` to drop it, and the URL is the filter — send it to someone.
- **People, not machinery.** A run a new version started is not here: it says
  `by new version of repo` on the run itself. Keeping the log to what people
  did is what stops a busy poll pushing their actions out of a log bounded by
  count.
- **Destroying a pipeline does not erase its history.** Its lines stay, under
  the name it had, marked `gone`, and the destroy is one of them. A rename
  keeps its history too, and answers to the new name.
- **Only what happened is recorded.** A refused action — a read-only server,
  a pipeline that would not load — leaves no line, and neither does a
  decision that changed nothing: a second operator approving what the first
  already rejected, a double-clicked answer, or a form naming an approval or
  question that no longer waits is settled on with the usual redirect, and
  only the decision that took effect is in the log.
- **Bounded by count**, `--activity-history` (default 10,000), the oldest
  dropped first, whichever pipeline they belong to. Names in it are personal
  data; that bound is how long they are kept.
- On a server nobody signs in to, the who is `—`.

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

A task's output arrives the same way — a task with `assert:` or `fix:` too —
and so does what a get's `in:` or a put's `out:` prints to stderr; their
stdout is not recorded, because `out:`'s is the version the resource protocol
reads, not something to show. A `check` records nothing. While the command
runs, what it prints is recorded a line at a time (a partial line is
published anyway once it has waited a second and the command writes again) and
appended to the step's row as it lands, so a six-hour build is readable while
it builds. When the command exits, those pieces are replaced by one record of
its output, bounded at 32KB: the first 8KB and the last 24KB, with a line
between them saying how much was left out. The end is kept because the end is
where a failing command explains itself; the pieces are dropped because a
finished build should cost the same on disk however much it printed. A build
that dies mid-step keeps them, as the only record of what that step said. A
progress bar's carriage returns are collapsed to the value a terminal would
show, one per piece, rather than kept as the thousands of lines they would
otherwise be.

What the bound leaves out is not lost. A step that prints past it has every
byte the record would have held without the bound stored in the state
database, in 256KiB rows keyed by run, step and `n` (which counts that step's
kept outputs, one per attempt), and the run page's `full output` link streams
them back in order. Nothing is stored until the output has outgrown the
record, so a step that fits costs no extra rows, and there is no file or
directory to configure: sqlite and Postgres hold it alike, and `steps run` and
`steps test` keep it in the state file beside the pipeline the same way. The
rows go with their run (a run `run_history:` reaps, or a destroyed pipeline's
runs; see [infra.md](infra.md#how-much-history-to-keep-run_history)), so
nothing sweeps them separately. They are real database rows: a runaway step
costs up to 64MiB of state until its run is reaped, and a lower `run_history:`
bounds that.

## Language

The browser UI speaks English, Simplified Chinese (`zh-CN`), Japanese (`ja`), Spanish (`es`) and Brazilian Portuguese (`pt-BR`). The page uses the `steps-lang` cookie if the reader set one, then the browser's `Accept-Language`, then English. A browser asking for a close relative gets the nearest language: Traditional Chinese (`zh-TW`, `zh-HK`) gets `zh-CN`, `pt` and `pt-PT` get `pt-BR`, and Galician (`gl`) gets `es`. The footer's picker (`auto` and one link per language, each in its own name) sets or clears that cookie, and `auto` goes back to asking the browser.

Only the browser UI is translated. The command line, logs, these docs, pipeline YAML, and the messages the `/api` routes return stay in English, and where one of them shows on a translated page it is marked `lang="en"`. The words `pipeline`, `job`, `step`, `resource`, `agent`, `task`, `get` and `put` stay English everywhere, because they are what people type in YAML. Durations, timestamps and counts such as `4m 05s` and `34,500` are spelled the same in every language.

## Following a run you started

Triggering does not drop you back on a list to refresh. A trigger lands on a
short waiting page that reports what the queue is doing and forwards itself to
the live transcript the moment a worker picks the job up — a queued job has no
run id until then, which is why there is a waiting room rather than a
redirect. While it waits it lists what the drain checks before claiming, each
line `✓` clear or `⟳` blocking — the pipeline is not paused, its serial group
is free, its `max_in_flight` has room, a worker has claimed it — so the first
blocking line answers why it has not started, as Concourse's "preparing build"
does. It lists only checks the drain makes: a held job is not one, because a
person's trigger runs a held job.

Every status is one word, one glyph and one colour, on every page:

| State | Of | Drawn | Meaning |
|---|---|---|---|
| queued | run | `○` faint | triggered, not yet claimed by a worker |
| running | run | `◐` yellow | in progress |
| passed | run | `✓` green | every step succeeded |
| failed | run | `✗` red | a step said no |
| errored | run | `!` red | the machinery broke: an image, a daemon, a provider |
| aborted | run | `■` dim | a person stopped it; not a failure |
| paused | pipeline | `⏸` blue | a person paused it; an unpaused pipeline reads *active* |
| held | job | `⊘` red | the [circuit breaker](infra.md#circuit-breaker-max_consecutive_failures) stopped its automatic triggers |
| unreported | step | `?` dim | the run ended before this step reported how it did |

### The tab mark

The browser tab carries a **mark** for whatever the page is about: a run page
marks that run, every other pipeline page marks the pipeline, and the root
marks every pipeline at once. The favicon is a disc colored by status, with a
yellow ring around it while something is running — so a red disc with a ring
reads "broken, and a fix is building". The title leads with the disc's glyph
alone, since text cannot circle it; the icon beside it carries the ring.
A pipeline is colored by each job's latest *finished* run: red when any of
them failed or errored, blue while paused, green otherwise. The mark follows
the page as it refreshes, and a run page's mark changes the instant the run
ends, so a tab left in the background reports the outcome without being
reopened.

The jobs graph refreshes itself every couple of seconds, in place — it keeps
your scroll position rather than reloading the page — and pauses while the
tab is hidden.

## The action bar

Every action on a pipeline's pages lives in one bar under the nav, in the same
place on every page: the pipeline's **Pause** at the left, what this page's job
or run can have done to it at the right. Each button's tooltip says what it
does. A paused pipeline turns the bar blue and its button reads **Unpause**;
buttons that would start something are disabled, and say why when hovered.

| Page | Right-hand buttons |
|---|---|
| a job | **↻ Trigger**, and **Release** when the breaker holds it |
| a run, finished | **↻ Trigger** (a new run of its job) and **⟲ Retry** (this run again) |
| a run, failed | as finished, plus **⏵ Resume from *step*** when the run can continue where it stopped |
| a run, live | **↻ Trigger** and **■ Abort** |
| the follow page | **■ Abort** the queued run |

## Triggering, retrying, approving, releasing

Eleven controls, each doing what a CLI verb does:

- **Trigger** enqueues the job into the durable trigger
  queue `steps web` uses — the same queue this process's own polling fills.
  `steps web` drains it in-process by calling `pipeline.RunJob` — there is no
  second execution path, so a job run from a browser gets the same caching,
  hooks, serial groups, and recording as any other. A trigger builds the newest versions the job has not built, and honors the cache,
  which on an unchanged pipeline correctly does almost nothing; Retry, or `steps run --force` from a terminal, is how to run unchanged steps again. A job the breaker holds is still triggered, since the breaker stops only automatic triggers.
- **Retry** is `fly rerun-build`: a new run, the whole plan from the top, against exactly the versions the run was created with — what `steps run --rerun <run>` does. A `version: every` job builds each version as a run of its own, so Retry rebuilds that one version. Nothing that arrived since joins it, `passed:` is not asked again, and it takes no version, so it disturbs no other build. It skips the step cache, since a retry of a run that passed would otherwise do nothing. A run preflight refused recorded no versions, so its retry resolves afresh and takes what it builds — the run it would have been. The new run links the run it retried, and a retry of an old run does not become its job's status — only a retry of the job's latest run does, as in Concourse — though the job is drawn running while any retry runs, as Concourse's is. See [conformance.md](conformance.md).
- **Resume from *step*** continues a failed run from the step that failed, in the run's own tree, skipping every step it already finished — what `steps run --resume <run>` does. It is the same run, put back in flight, not a new one, so the button lands on the run's page. The button names the step the run picks up at, and its tooltip how many finished steps it keeps; it is offered only when there is something to keep and somewhere to keep it: the run failed, finished at least one step after its first `get`, its tree is still on disk, and no `pipeline set` since has moved a finished step from its place (a fix to the failed step keeps the button). A run whose tree retention has removed offers only Retry, and asking anyway (`steps runs resume`, or a tab left open) is refused 409 rather than quietly building from the top — the full run a person pressing Resume chose not to pay for. The same holds if the tree goes between the press and the claim: the queued resume fails, naming Retry. Concourse has no resume; it exists for the plan whose sixth agent fails after five paid for themselves — an exhausted provider key, a rate limit — where Retry would spend all five again. A job with no `get` is not offered one.
- **Approve / Reject** on an `approval:` step, with the reason recorded — the
  same row `steps approvals approve` writes, under the name of whoever signed
  in (a decision on a server nobody signs in to reads `by web`).
- **Answer** an `ask_user` question a step is parked on — one click for an
  offered option, or your own words — the same row `steps questions answer` writes. See
  [agents.md](agents.md).
- **Release** a job the circuit breaker is holding — what `steps jobs release` does.
- **Pause / Unpause** the whole pipeline — what `steps pipeline pause` throws.
  The action bar carries it on every page of the pipeline, and the root lists
  it for every pipeline at once. Each puts you back on the page you pressed it
  from.
- **Pause all / Resume all**, in the root's action bar (blue while it holds), stops every pipeline at once, as a
  stop gap. It does not touch any pipeline's own pause: while it holds, each
  row and each pipeline's button still say that pipeline's own state, and
  **Resume all** hands every pipeline back exactly that — one paused before
  stays paused. It survives a restart, needs the admin role (it stops
  pipelines whose own rules may not let an operator touch them), and is
  logged as `pause all` / `unpause all` against every pipeline it reached.
- **Abort** a running run from its page's bar, or a queued one from the page a
  trigger lands on — what `steps runs abort` asks for. See
  [Aborting a run](#aborting-a-run).
- **Connect** an oauth `mcp_servers:` entry from the
  [mcp tab](mcp.md#authorizing-from-the-browser-the-mcp-tab), finishing the
  login in the browser you are already in.
- **Test** any declared `mcp_servers:` entry from the same tab — the only
  thing on that page that connects to anything.

`--read-only` withholds all eleven: the controls disappear from the pages and
the routes refuse. The mcp tab keeps its status column — that a server needs a
login is a diagnostic a build box should still show, and the row names the CLI
command instead. The queue is still drained, polling still runs, and
`steps pipeline set` still works — that flag is a statement about the
browser's surface, not about what the process does on its own or about how it
is deployed. It also serves no [`mcp_exports:`](mcp.md#sharing-tools-with-other-agents-mcp_exports),
since calling a tool is acting. `--listen 0.0.0.0:8088 --read-only` is a build box that still has
to notice new versions; read [Security](#security) before you expose one.

**The webhook route is the one exception, deliberately.**
`POST /p/<slug>/hooks/<resource>` still works under `--read-only`, and the job
it enqueues still runs. It is not a UI control: it carries the sender's own
signature, which is a stronger check than any control above has, and withholding
it would mean a read-only box could not be the thing GitHub notifies — which is
most of why a build box is exposed at all. `--read-only` says a *browser*
cannot start work here; it does not say nothing can. If that is what you
want, do not give the pipeline a `type: webhook` resource — with none, the
route is a 404. See [webhooks.md](webhooks.md#receiving-deliveries).

## Starting a run from the terminal

`steps run` runs a job on the machine you type it on. To have the **daemon** run it instead, with the logins it holds, in its queue and its history, ask it:

```bash
steps runs trigger -p app build                      # queue build, as the Trigger button does
steps runs trigger -p app review --pin number=87     # queue it against the version whose number is 87
steps runs retry   -p app 46UMHVPYRA6YHB7M           # queue a retry of a recorded run, as its Retry button does
steps runs resume  -p app 46UMHVPYRA6YHB7M           # continue a failed run from the step that failed, as its Resume button does
```

- A plain trigger merges into a run of that job already waiting, exactly as a second click of the button does.
- **`--pin`** takes the same `field=value` pairs `steps run --pin` takes, and is repeatable. A pinned trigger is a queue row of its own, because it builds something the job's ordinary trigger does not; asking for the same pin twice while it waits is still one run. The pin is stored with the row, so a daemon restarted before the run starts still builds what you asked for. A pin is matched against the versions already recorded first (on a `passed:` get, the ones upstream passed), and the check runs only when none matches — so a [webhook](webhooks.md) delivery, which no check can report, is rebuilt by pinning its `id`. In a job with several gets, the pin applies to the gets whose versions carry the pinned fields: `--pin number=87` picks the pull request, and an image beside it whose versions are `{digest}` takes its own `version:` pin or its latest. A pin no get takes fails the run rather than building the latest of everything.
- **A pin skips `passed:`**: the pinned run builds the version it names, whether or not the upstream jobs passed it. So on a job with a `passed:` get, an operator may only pin a version every job in that `passed:` has passed; pinning anything else needs the **admin** role and is refused 403 otherwise. Without `--role` rules everyone is an admin. Each constrained resource is judged on its own: a fan-in whose pinned versions passed upstream in different builds is not caught.
- **`--force`** skips the step cache for that run. It cannot be combined with `--pin`.
- **`resume`** continues a failed run in place, keeping what it finished; refused, naming Retry, when the button would not be offered.
- **`retry`** rebuilds a past run against the versions it was built with, which is Concourse's `fly rerun-build`.
- Each is refused where the button is: on a `--read-only` daemon, on a paused pipeline, for a job the configuration no longer names.

**`--follow`** (or `-w`, `--watch`, as in Concourse's `fly trigger-job -w`) waits for the run this trigger queued to start, then follows it to its end and exits as it did — 0 for succeeded, the failure code for failed, the abort code for aborted — so `steps runs trigger -p app build -w && deploy` deploys only on success. It follows exactly that run: if the trigger merged into a waiting one, two terminals that both asked follow the same run, and a newer run of the job is never picked up by mistake. If the run is aborted before it starts (`steps runs abort --queued`), the terminal says so and exits as an abort. Ctrl-C detaches and the run goes on, as with `steps runs follow`. Without `--follow`, the trigger returns at once; follow it later with `steps runs follow -p app --job build`.

### A one-off from a file

`steps run` with **`--target`** hands a job from a pipeline file to a daemon instead of running it here, like Concourse's `fly execute`, and follows it from your terminal:

```bash
steps run ci.yml --job build --target http://127.0.0.1:8088
export STEPS_TARGET="http://alex:$(cat .steps/web-password)@ci.example.com:8088"   # the password, out of the command line
steps run ci.yml --job build --target http://ci.example.com:8088
```

- The daemon validates the file as a set would, on its own machine, and serves it under a pipeline it names itself (`one-off.<random>`), so a one-off can never replace a pipeline you set. That pipeline is not polled: only the job you named runs, never what a `trigger: true` resource would start.
- The run is the daemon's — its workers, its logins, its history and its run page, which the terminal prints a link to. Ctrl-C detaches and the run goes on; the detach line names the `steps runs follow --target` that reattaches.
- The terminal names the one-off as soon as it is queued (`queued: build on … as one-off.<random>`). If its run is aborted before it starts — `steps runs abort -p one-off.<random> --queued build`, say, while it waits behind `--max-runs` — the terminal says so and exits as an abort.
- **`--pin`** and **`--force`** go to the daemon and mean what they mean on `steps runs trigger`. `--force` matters even on a new pipeline, because every one-off files its step cache under one scope shared by all one-offs, rather than under the name minted for it: a second one-off of the same file is answered from the first's outputs unless forced. Together they are refused before anything is sent, since the daemon cannot skip the cache for a pinned run.
- The daemon keeps the last **`--one-off-history`** finished one-offs (default 10) — each one's run page, transcript and `steps runs -p one-off.<random>` entry. Once a newer one-off finishes past that count, the oldest finished one is no longer served and everything it recorded is deleted. A one-off still running is never counted or forgotten, and one retried from its page counts as finished when the retry does. `0` keeps every one. A restart serves the kept one-offs again, still unpolled, and resumes any interrupted run. (Concourse keeps `fly execute` builds forever, since its build-log retention only trims job builds; here a one-off is a whole pipeline in the list, so it is bounded.)
- It needs the **admin** role, because it is arbitrary commands on the daemon's machine — what a set is. Names starting `one-off.` are the daemon's own: a set or rename onto one is refused.
- Only the file and its `run_file:` includes travel, unless you send folders with `--input` (below). `--resume`, `--replay`, `--rerun` and `--db` do not apply.

**`--input GET=PATH`** uploads a folder from your machine and uses it in place of that `get`, as Concourse's `fly execute -i` does — so the run sees your checkout, edits not yet committed included, and a `get` of a path only your laptop has (`uri: /Users/...`) can run on a daemon elsewhere:

```bash
steps run ci.yml --job build --target http://127.0.0.1:8088 --input src=.
steps run ci.yml --job build --target http://127.0.0.1:8088 --input src=. --input config=../ci-config --exclude node_modules
```

- Any `get` in the job may be replaced, one `--input` each. One the job does not have is refused before anything is sent.
- The replaced get's resource is neither checked nor fetched for the run. The folder is a one-off version of that get: it is never added to the resource's versions, never counts as having passed the job, cannot trigger anything, and never becomes the newest version a later run picks up. Its version is the hash of what was sent (`{"upload": "<sha256>"}`), so a second upload of the same files is answered from the step cache, as any repeated work is — `--force` re-runs it.
- **Files git ignores are left out**, as in Concourse: inside a git repository, what is sent is what `git ls-files` lists — tracked files still on disk, plus untracked files no `.gitignore` excludes. **`--include-ignored`** sends the whole folder instead (the `.git` directory included). Outside a repository, the whole folder is sent.
- **`--exclude GLOB`** (repeatable) leaves out what matches, by path or by name: `--exclude node_modules` drops that directory wherever it is at the top, `--exclude '*.log'` every log file, `--exclude 'build/*'` what is under `build/`.
- Empty directories are not sent, as git does not keep them either. Outputs do not come back (Concourse's `-o` is not offered).
- The daemon reads every folder before the run is queued, and refuses the lot — starting nothing — once they unpack past **`--one-off-input-max-bytes`** (default 1 GiB, `0` is no limit). The folders are kept with the one-off and deleted when it is forgotten, so a restart that resumes an interrupted one-off resumes it on what it was sent.
- `--input`, `--include-ignored` and `--exclude` apply only with `--target`; a local `steps run` already reads your folders.
- `--target` is read from the flag only, never from `STEPS_TARGET`: exported for the control verbs, it would quietly send every local run away. Its credentials are the exception: a `--target` without any takes `STEPS_TARGET`'s when both name the same scheme and host, which keeps the password out of the command line ([authentication.md](authentication.md#from-the-cli)). `steps runs follow --target` does the same.

**`steps runs follow --target <daemon> -p <name> [run]`** follows a run through the daemon's API rather than the state file, which is how a daemon on another host is followed. It reads one page of events at a time, and each page asks the daemon's access rules again — a pipeline's `access:` hides those reads exactly as it hides its pages.

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
- A step that runs on the daemon's own machine (no `image:`) is stopped by
  killing its process group. A process the step moved out of that group on
  purpose — `setsid`, a daemonizing `nohup … &`, anything that starts a new
  session — is not in it and outlives the abort, reparented to init, as it
  would outlive a Ctrl-C of `steps run`. A container step has no such gap:
  its container is stopped. Run a step that starts background services in
  an `image:`.
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
steps web --max-concurrent 4   # up to four queued jobs at a time, per pipeline
steps web --max-runs 6         # and at most six runs at once across all of them
```

`--max-concurrent` is per pipeline; `--max-runs` is the whole daemon's. A
triggered run that a limit holds back — either flag, a job's `serial:` or
`max_in_flight:`, a paused pipeline — stays **pending**: a row in the queue,
not a run yet, and the follow page names which limit is holding it. A pending
run has not started, so a restart keeps it and runs it; a build that was
going is resumed instead (see [What a restart does](#what-a-restart-does)).

**There is no one-shot mode.** A process that never binds has nothing to be
set into, and a server is its own scheduler — run it under systemd as a
service rather than a timer, and let `--interval` be the schedule. A job that should run at a
time of day gets a [`cron` resource](resources.md#the-built-in-cron-type); the
interval is only how often that clock is read.

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
- **Preflight runs before the first poll**, the same check `steps run` does,
  with one asymmetry: a problem *waiting cannot fix* — an `mcp:` tool the
  server does not expose — stops that pipeline's polling and says so, while a
  problem waiting might fix — a server that did not answer, a token a refresh
  would renew — is logged once as `trigger.preflight_transient` and left to
  the loop, which retries by its nature. `--no-preflight` skips the
  check entirely. It runs inside the poller, so it never delays serving.

## Applying a change

Set it again. There is no watcher and no `--watch`:

```bash
steps pipeline set -c pipeline.yml       # edit, set, and it is serving
```

**A save-to-apply loop is a one-line wrapper** around this — `watchexec -w
pipeline.yml -- steps pipeline set -c pipeline.yml -n` — and that is where it
belongs. A watcher inside the daemon would be a set that fires with nobody
attached; a configuration the daemon will not accept is refused to the
terminal that set it instead.

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

**Where build trees live is the daemon's, not the pipeline's.** Every pipeline
a daemon serves builds under its `--workspace-root`, which defaults to
`workspace/` beside its sqlite state file (`.steps/workspace` for the default
database, and for a postgres one, in the working directory), so a daemon always
keeps the step cache, task caches and resource cache across builds. A set
never moves it, so a set never deletes the tree a build is working in. A root
the daemon cannot write refuses every set, naming the root.

## One database, several pipelines

A daemon holds every pipeline set into it in ONE state database — `.steps/steps.db`
unless `--db` names another:

```bash
steps web --db /var/lib/steps/state.db
steps pipeline set -p app -c app.yml
steps pipeline set -p infra -c infra/pipeline.yml
```

A bare path is a sqlite file, and so is `sqlite:///var/lib/steps/state.db`
— the scheme chooses the driver, the way `--worker` takes `ssh://` and
`aws://`, and `postgres://` is the other one (below). A scheme no driver
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

WHEN                 PIPELINE  JOB      STATUS     RUN               CONFIG
2026-08-30 09:14:02  infra     deploy   succeeded  UNVFHMCHVWY6GV6N  9b1e04c7d2aa
2026-08-30 09:12:40  app       build    failed     46UMHVPYRA6YHB7M  3f6a91d0be52

break one down with: steps runs cost -p <pipeline> <run> --db /var/lib/steps/state.db
```

`PATH` is where the configuration was last set FROM — a file on whoever's
machine ran `set`, recorded so a reader can tell two checkouts apart, and never
opened here.

**The read commands take `-p <name>`, not a path**, because a served pipeline
has no file on this machine:

```bash
steps runs -p app                      # what ran
steps runs steps -p app                # why a step did what it did
steps runs follow -p app               # watch the newest run to its end
steps runs cost -p app 46UMHVPYRA6YHB7M
steps approvals -p app
steps questions -p app
steps jobs -p app
```

They read the database directly rather than going through the daemon, so they
work against a stopped one — and they take `--db` when it is not the default.
`steps run`, `steps test`, `steps validate` and `steps plan` still take a file
path and still default to `.steps/<filename>.db` beside it: those are the local
commands, and a local command has a file. (`steps run --target` sends that file
to a daemon instead; see [A one-off from a file](#a-one-off-from-a-file).)

Run ids stay globally unique, but `--resume` and `--replay` still refuse an id
belonging to a different pipeline in the same file: continuing another
pipeline's run would reuse its workspace and step indexes against this
pipeline's plan.

**A database an older steps wrote is upgraded when it is opened**, by any
command that writes to it (`steps web`, `steps run`, `steps test`). Before upgrading a file, steps copies it beside
itself as `<file>.v<old schema>.bak`, keeping only the newest copy; if the
upgrade fails, the file is left as it was and the error names the copy. A
read command (`steps runs` and the like) does not upgrade, and says which
command will. Two cases are still refused: a file older than v0.1.0,
whose answer is deleting it (losing run history and cache, nothing else), and
a file a NEWER steps wrote, whose answer is that steps.

### Postgres

History can live in a Postgres server you already run instead of a file on the
daemon's disk — same commands, same web UI, same pipelines:

```bash
export PGPASSWORD=…
steps web --db 'postgres://steps@db.internal:5432/ci?sslmode=verify-full'
steps runs --db 'postgres://steps@db.internal:5432/ci?sslmode=verify-full'
```

`postgresql://` is the same thing, and a unix socket is
`postgres:///ci?host=/var/run/postgresql`. Only the url form is accepted, not a
`key=value` string. Quote the url: its `?` and `&` mean something to a shell.

- **The database must exist; steps never creates it.** Create it with
  `createdb` first. Inside it, steps creates and uses one schema, `steps`, and
  touches nothing outside it — so it can share a database with an application
  that has its own `runs` table. `search_path=<schema>` in the url picks
  another schema (never `public`, which is refused); each schema is its own
  state database, so two daemons can share a server as long as each has its
  own.
- **Credentials do not belong in the url.** A password in `--db` is kept by
  every process listing and shell history that saw the command, and steps warns
  when it sees one. Set `PGPASSWORD`, or use `~/.pgpass` or `PGSERVICEFILE`.
  Nothing steps prints — a banner, an error, a pasteable hint — carries the
  password, and the hints drop it from the url they print.
- **Off the machine, use `sslmode=verify-full`.** The default is libpq's
  `prefer`, which quietly falls back to an unencrypted connection.
- **Behind PgBouncer in transaction mode**, add
  `default_query_exec_mode=simple_protocol`: prepared statements do not survive
  a pooler that hands each transaction a different server connection. List
  `search_path` in PgBouncer's `track_extra_parameters`, not
  `ignore_startup_parameters`: a session that lost it is refused on open,
  since steps' tables would otherwise land in `public`.
- **Still one `steps web` per state database** — per schema, here. Nothing
  arbitrates two daemons sharing one, exactly as with a file.
- **The first open creates the schema; every later one only reads its
  version**, so once it exists the role steps connects as needs no `CREATE`
  privilege — `SELECT`, `INSERT`, `UPDATE` and `DELETE` on the schema's tables
  (and `USAGE` on its sequences) are enough, and a role with only `SELECT` can
  run the read commands.
- **A schema an older steps wrote is upgraded on open, in one transaction**,
  so a failed upgrade leaves it as it was (Postgres DDL is transactional; take
  your own backup if you want one). Upgrading needs the role to own the
  tables, which a role with only `SELECT`/`INSERT`/`UPDATE`/`DELETE` does not —
  open it once as the owner. A schema older than v0.1.0, or one a newer steps
  wrote, is refused; for the first, `DROP SCHEMA steps CASCADE` and run again,
  which loses steps' run history and cache and nothing else in the database.

## Security

On the loopback address it binds by default there is nothing to authenticate
against: this is the local runner's own front end, in the same trust domain as
the shell that started it. A daemon reachable from anywhere else turns on
accounts — local ones, or GitHub or Google sign-in — and roles that say who may
do what, on every route but a few that carry their own credential (the webhook
one among them). See [authentication.md](authentication.md).

**Two browser-reachable routes make the daemon act on the outside world**, and they are the only ones that do: the mcp tab's **Connect** starts an oauth login and **Test** connects to a declared server. Both need the daemon's credentials, both are refused cross-origin, neither can name a server the pipeline does not already declare, and `--read-only` withholds both while keeping the page. The reasoning is in [authentication.md](authentication.md#what-a-browser-may-start).

**`steps pipeline set` is a remote-shell endpoint. Say that plainly: a
pipeline is arbitrary commands, so anyone who can reach this port can run
anything they like as the user running the daemon.** With no accounts
configured there is no token, no password, no allow-list; with them, it takes
the `admin` [role](authentication.md#who-may-do-what-roles). `--read-only` does
not close it either — that flag has always been
a statement about the *browser's* surface, and the set endpoint is the
deployment path, not a button on a page.

That is the reason for the loopback default, and it is a stronger reason than
the trigger controls ever were. **Binding to a routable address hands the
machine to whoever can reach the port.** `--listen 0.0.0.0:8088` exists for
someone who has decided that is what they want; put credentials on it
([authentication.md](authentication.md)) and TLS in front of it, or put it
behind something else that authenticates — an SSH tunnel, a reverse proxy, a
network nobody else is on — and treat `--read-only` as being about the browser
only.

**Loopback keeps other machines off the port, not other web pages.** A page
open in a browser on this machine can re-point its own hostname at `127.0.0.1`
after it loads — DNS rebinding — and from then on its requests reach this port
carrying that name as both `Host` and `Origin`, which is exactly what a
same-origin request looks like. So everything under `/api/` — what
`steps pipeline` and `steps runs abort` talk to — refuses a request carrying a
header only a browser attaches: an `Origin` of any value, which every browser
sends on the `PUT`, `POST` and `DELETE` those verbs use, or a `Sec-Fetch-Site`
saying anything but `none`, which is a URL typed into the address bar. The CLI
sends neither, and no page in the UI calls `/api/`.

**On a loopback `--listen`, other names are refused by default.** A browser
can reach a daemon bound to `127.0.0.1` by a loopback name and by nothing
else, so a request carrying any other `Host` is a rebinding page and gets a
403 before it reaches a page or a button. Two names pass anyway: the host of
`--external-url`, which is what a reverse proxy on this machine forwards, and
anything on a webhook delivery route (`/p/<pipeline>/hooks/<resource>`),
because a tunnel forwards the sender's own name and a delivery is admitted by
its signature, which a rebinding page has no more of than any other site.
Naming `--allowed-host` replaces this list with yours, hooks included. On any
other `--listen`, `Host` is not checked unless you name `--allowed-host`,
because a reverse proxy in front of the daemon forwards the name the client
used — only you know which names are this daemon's.

The browser's own controls — trigger, retry, approve, answer, release, pause,
abort, connect, test — are `POST`s refused when their `Origin` names another host, which stops another
site aiming a form at your port. A rebinding page is not another host: on a
non-loopback `--listen` it can read every page, configurations included, and
press those controls, unless `--allowed-host` refuses its name or
`--read-only` has withheld them. It cannot set, destroy or rename a pipeline:
those verbs live under `/api`, which refuses anything browser-shaped.

**`--allowed-host` closes rebinding.** Name every host this daemon is reached
by — the one in `--external-url`, the one a reverse proxy forwards, the one a
webhook provider was given — and any request whose `Host` is not one of them,
or a loopback name (`localhost`, `127.0.0.1`, `::1`), is refused with a 403
before it reaches a page, a button or `/api`. Repeat the flag, or put a
comma-separated list in `STEPS_ALLOWED_HOSTS`. A port is ignored, and so is
case. It applies to every route, webhooks included — a delivery that names
another host is refused like anything else — except `/healthz`, which a
platform's probe reaches by IP and which answers `ok` and nothing more.

```
steps web --listen 0.0.0.0:8088 --allowed-host steps.example.com
```

**Pause and unpause are on the browser's side of that line, deliberately.**
They are the one pair a page can reach that `/api` also serves, and the reason
is that the envelope does not grow: a page that can press **Trigger** already
runs arbitrary commands as this user, so unpausing grants it nothing it did not
have, and pausing is a smaller stop than **Abort** next to it. What is *not*
on this side is everything that changes what a pipeline IS.

**What an anonymous caller can make the daemon hold is bounded.** A request
body is capped at 1 MiB, except a `steps pipeline set` under `/api` (8 MiB)
and a webhook delivery (bounded by the resource that reads it). An approval's
reason and a question's answer are capped at 64 KiB each, from the browser and
from `steps approvals`/`steps questions answer` alike. In-flight sign-ins,
consents and authorization codes are each held to at most 1,000 entries; a
flood of them evicts the oldest rather than growing without end. A sign-in
start is also held to 32 in flight per address the connection came from (an
IPv6 address counts by its /64), past which an address displaces only its own,
so a flood from one place cannot push out anybody else's sign-in. Behind a
proxy every visitor shares the proxy's address and that cap. A live stream
is authorized when it opens and re-checked every 30 seconds, so a sign-out, a
removed account or a narrowed `access:` rule stops it without waiting for the
run to end.

## Configuration file

Every `steps web` flag can also live in one YAML file, under a `web:` key,
given with `--config` or `STEPS_CONFIG`:

```sh
steps web --config /etc/steps/steps.yml
```

```yaml config
web:
  db: postgres://steps@db/steps
  listen: 0.0.0.0:8088
  external-url: https://ci.example.com
  allowed-host: [ci.example.com]
  interval: 1m
  workspace-root: /var/lib/steps/workspace
  resource-cache-max: 200
  worker:
    gpu: ssh://ci@gpu-box
  role:
  - admin=Provider == "github" && "platform" in Groups
  - viewer=Provider == "github"
```

**Every key is exactly one flag**, spelled as the flag is (`external-url`, not
`externalUrl`), so nothing can be set in one place but not the other. A
repeatable flag takes a list; a `name=value` one (`--worker`, `--pin`) takes a
map. A key that is not a flag is a startup error, never silently ignored. A
path in the file (`local-users-file`) is resolved against the working
directory, not against the file.

**Precedence is flag, then file, then environment variable, then default.** A
flag on the command line always wins, so `--listen` on the line beats the
file's; the file beats `STEPS_ALLOWED_HOSTS` and the other `STEPS_*` variables.

**The file is a privilege boundary.** It will name commands the daemon runs
as its own user, so it is held to what sshd asks of its own config: refused if
it is writable by group or others, or owned by another user. It is never
discovered from the working directory — a cloned repository's `steps.yml` is
just a file — only `--config` or `STEPS_CONFIG` names it.

**It never holds a credential.** The file is meant to be committed and
reviewed. `basic-auth-password` is refused in it and stays
`STEPS_BASIC_AUTH_PASSWORD`; the OAuth client secrets already come from the
variables `--github-client-secret-env` and `--google-client-secret-env` name.

**`secret:` and `allow-unfenced:` are the keys that are not flags**, both
nested objects no flag can carry; `allow-unfenced:` is [below](#opting-out-two-keys). `secret:` names the commands the
daemon runs to fetch a credential.
A daemon with a secret source refuses to start unless somebody has to sign in
(`--local-user`, `--basic-auth-username` or a sign-in provider), loopback
included: a daemon nobody signs in to treats every request as admin, and every
step is handed `STEPS_URL`, so any step could set a pipeline that reads another
pipeline's secrets.

`steps web` reads all of it. `steps validate` reads only `fence:` and `allow-unfenced:` (below) and,
with `--live`, `secret:`. `steps run` and `steps test` take `--config` too (and
never `STEPS_CONFIG`) but read only `secret:`,
so a pipeline with secrets can be run without a daemon; see
[secrets.md](secrets.md#running-from-the-cli).
`steps pipeline set` and the rest take their flags on the command line as
before.

## Fencing commands (`fence:`)

Whether a command runs fenced is up to whoever wrote the pipeline, and a
pipeline somebody else sets runs as the daemon's own user. `fence:` is the
operator's say in it:

```yaml config
web:
  fence: warn
```

- **`off`** (the default) changes nothing.
- **`warn`** accepts every pipeline, and lists each command that would run
  **unfenced**: in the answer of a `steps pipeline set` that sends a change,
  under a `warning:` line (a set of the file already served sends nothing and
  says `unchanged`), and in a notice at the top of the pipeline's page. A pipeline restored at
  startup is judged the same way, so the notice is there after a restart too.
- **`enforce`** refuses such a pipeline at `steps pipeline set`, naming every
  unfenced command and what would fence it, unless [both keys](#opting-out-two-keys)
  opt it out. A pipeline restored at startup is judged the same way: one that
  no longer complies, because the allowlist changed or this machine has no
  sandbox, is shown **broken** with the reason, as a `--worker` change would
  leave it, until a set repairs it or the daemon restarts with a policy it
  meets. Roll out `warn` first: under `enforce` every stored pipeline that runs
  something unfenced is broken on the next restart. A `steps run --target`
  one-off is judged too, under the name the daemon mints for it, which no
  allowlist can name, so under `enforce` it runs only what is fenced.

A command is **fenced** when it runs in an `image:` without `privileged: true`,
or under [`sandbox: true`](infra.md#host-sandbox-sandbox) on a daemon whose
machine has the sandbox: darwin, or linux with Landlock (kernel 6.2+). Elsewhere,
a linux kernel without Landlock included, a sandboxed command is listed.
A step placed on a `docker+ssh://`, `aws://`, `gcp://`, `do://` or `local:`
worker runs in its image, so it counts as fenced; one on a bare `ssh://` worker
does not. Anything that runs no command has nothing to fence: an `expr:`
resource, steps' other built-ins, `approval:`, `load_var:`, an agent whose
tools touch no files and run no shell, until it carries a `when:` guard.

Everything else is listed, each place named with what runs there: a task or
agent step with no `image:` or `sandbox:` (job hooks included), a container
with `privileged: true`, a resource whose type runs `check`/`in`/`out` on the
host (the built-in `git` type does, until a `resource_types:` entry of its
name gives it an `image:`), a `github-prs`/`github-comments` resource with
`checkout: git` and no such image, every stdio `mcp_servers:` entry without `image:` (one that runs `docker` itself included), an
`mcp_exports:` entry whose custom `run:` tools or exported agents have no
`image:`, an agent whose sub-agent runs host tools under no `sandbox:` of its
own (a step's `sandbox: true` does not reach it), a `when:` guard on a step
with no host command of its own (a block, `approval:`, `load_var:`, an agent
whose tools touch nothing, a `put:` of a built-in type), which runs on this
machine unless a block around it, or the block itself, says
[`sandbox: true`](infra.md#on-a-block), and a CLI agent with `settings: project`, whose repository hooks run
on this machine whatever fences its tools.

Because the policy lives in the daemon's config file, a pipeline cannot loosen
the rule that judges it. To see the list before a set, give `steps validate`
the same file: `steps validate --config /etc/steps/steps.yml pipeline.yml`
prints it as warnings and still passes under `warn`, and fails as the set
would under `enforce`. It judges the pipeline under the name it would be set
as (the file's name, or `--name app=pipeline.yml`). It reads only `fence:`,
`allow-unfenced:` and, with `--live`, `secret:`, and judges `sandbox: true` by
the machine it runs on, so validate on the daemon's machine (a darwin laptop
does not see what a linux daemon without Landlock lists).

### Opting out: two keys

Some commands cannot be fenced, or are not worth it: a task that posts with
the machine's own `gh` login, say. Under `enforce` such a command runs only
when **both** of these say so:

1. **The pipeline declares it**, with `unfenced: "<reason>"` on the entity, the
   place `image:` or `sandbox:` would go. The reason is what everyone reading
   the pipeline, its diff and its page sees.
2. **The daemon's config file allows it**, under `allow-unfenced:`, keyed by
   the pipeline's name:

```yaml config
web:
  fence: enforce
  allow-unfenced:
    release: [task notify, task tag, guard ship]
    review: [agent coder, mcp_server gopls, resource_type git]
```

Either key alone does nothing: a pipeline's own `unfenced:` would be the
pipeline vouching for itself, and an allowlist alone would hide why. The
config file is the privilege boundary (owner-only, never read from a
repository), and `allow-unfenced:` can only be set there, never as a flag.

Each entity is a kind and a name. `warn` and `enforce` print it in brackets at
the end of every listed line, so copy it from there:

| Entity | `unfenced:` goes on |
|---|---|
| `task <name>` | the `tasks:` entry, or the step (the only place an inline `run:` has) |
| `agent <name>` | the `agents:` entry, or the step; it also covers an `mcp_exports:` entry's exported agent |
| `resource_type <name>` | the `resource_types:` entry; for a built-in such as `git`, an entry of its name with only `unfenced:` (it stays the built-in) |
| `mcp_server <name>` | the `mcp_servers:` entry |
| `mcp_export <name>` | the `mcp_exports:` entry, for its custom `run:` tools |
| `guard <job>` | the step whose `when:` guard has nothing else to run under (a block, `get`, `load_var:`, `approval:`) |

```yaml
tasks:
  - name: notify
    unfenced: posts with this machine's own gh login
    run: echo notified

jobs:
  - name: ship
    plan:
      - task: notify
      - task: tag
        unfenced: tags the operator's checkout in place  # an inline run: says it on the step
        run: echo tagged
      - do:
          - task: notify
        unfenced: the release lock lives on this machine  # covers this block's guard
        when: test -d .
    assert:
      execution: [notify, tag, notify]
      outcome: succeeded
```

```yaml noexec=stdio-mcp
mcp_servers:
  - name: gopls
    command: gopls
    args: [mcp]
    unfenced: reads the module cache the operator keeps warm

mcp_exports:
  - name: lint
    access: "true"
    unfenced: runs the linter the operator installed
    tools:
      - name: golangci
        description: lint the repository
        run: golangci-lint run

resource_types:
  - name: git
    unfenced: clones with the host's ssh agent

resources:
  - name: repo
    type: git
    source: {uri: https://github.com/jtarchie/ci.git}

agents:
  - name: coder
    source: {model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY}
    unfenced: needs the host's go toolchain
    tools:
      - run_shell
      - mcp: gopls

jobs:
  - name: review
    plan:
      - get: repo
      - agent: coder
        inputs: [repo]
        dir: repo
        messages:
          - "Review the change."
```

- **It weakens nothing on its own.** Under `off` it does nothing, and under
  `warn` it is shown beside the listed command.
- **The reason cannot be blank**; `unfenced: " "` is a load error. On a step
  that runs no command and has no `when:` guard it covers nothing and is a load
  error too, and on a `put:` it belongs on the resource type, whose commands the
  put runs.
- **A rename drops off the allowlist.** A renamed task, agent, resource type,
  server, export, job or pipeline (`steps pipeline rename` included) no longer
  matches its entry, and is refused until the operator allows the new name.
- **An agent with a sub-agent** that runs host tools under no `sandbox:` of its
  own is listed under the agent, whatever fences the agent: its `unfenced:` and
  its entity are the agent's, since a block's or step's `sandbox: true` does not
  reach the sub-agent.
- **Never hashed**: adding or editing a reason re-runs nothing.

## Flags

`steps web` — facts about the box, all of them:

```
--config         a YAML file of these flags under a web: key ($STEPS_CONFIG;
                 see Configuration file)
--listen         address to serve on (default 127.0.0.1:8088)
--external-url   the URL published to every step as STEPS_URL (default http://<listen>;
                 unset for a wildcard listen address). http(s) only, no credentials,
                 query or fragment: credentials would land in every step's environment
--interval       how often to poll trigger: true resources (default 30s)
--secret-cache-duration
                 how long a secret looked up for a poll or a webhook delivery
                 is reused before its source is asked again (default 1m)
--max-concurrent maximum queued jobs running at once, per pipeline (default 1)
--max-runs       maximum runs at once across every pipeline (default 0, no limit)
--pin / --force  pin a version field; ignore the step cache (not the every-cursor)
--no-preflight   skip the pre-poll health check of models and MCP servers
--workspace-root where build trees, the step cache, task caches and the resource
                 cache live ($STEPS_WORKSPACE_ROOT; default workspace/ beside the
                 sqlite state file, .steps/workspace for a postgres --db)
--resource-cache-max
                 how many fetched resource versions the root keeps, least
                 recently used evicted first (default 50)
--store-max-bytes
                 how many bytes of cached trees the root's store keeps, least
                 recently used evicted first (default 8 GiB)
--fence          off (default); warn to list every command a pipeline would
                 run unfenced, on set and on its page; or enforce to refuse it
                 unless both keys opt it out (see Fencing commands)
--read-only      serve without trigger, retry, approve/reject, answer, release,
                 pause, abort, connect or test controls, and serve no mcp_exports
                 (steps pipeline set is NOT withheld — see Security)
--basic-auth-username / --basic-auth-password
                 one account, as a plain pair; both or neither,
                 env STEPS_BASIC_AUTH_USERNAME / _PASSWORD (see authentication.md)
--local-user / --local-users-file
                 local accounts as name:bcrypt-hash, or an htpasswd file of them
                 (see authentication.md)
--github-client-id / --google-client-id
                 sign people in with GitHub or Google; the secret is read from
                 $STEPS_GITHUB_CLIENT_SECRET / $STEPS_GOOGLE_CLIENT_SECRET (or the
                 variable --github-client-secret-env / --google-client-secret-env
                 names), and needs --external-url and --role
--role           who may do what: viewer, operator or admin, each a rule about
                 the signed-in person (repeatable; see authentication.md)
--mcp-access     who may connect an agent to read the daemon at /mcp, a rule
                 in --role's language (default: anyone with the viewer role;
                 needs somebody to sign in; see mcp.md)
--activity-history
                 how many entries the activity log keeps across every pipeline
                 (default 10000, 0 is no limit; see Activity)
--one-off-history
                 how many finished steps run --target one-offs stay served
                 with their runs (default 10, 0 is no limit; see A one-off
                 from a file)
--one-off-input-max-bytes
                 how many bytes the folders a steps run --target --input sends
                 may unpack to, all together (default 1 GiB, 0 is no limit)
--allowed-host   refuse any request whose Host is not this name (repeatable;
                 loopback names always pass; implied on a loopback --listen;
                 env STEPS_ALLOWED_HOSTS, comma-separated; see Security)
--run-history / --version-history
                 how many runs of each job / versions of each resource to keep;
                 a pipeline's defaults: wins (see infra.md)
--max-run-history
                 the most runs of any job to keep, whatever a pipeline asks,
                 its 0 included (default 0, no ceiling; see infra.md)
--artifact-store mirror cached step outputs to a content-addressed store,
                 e.g. s3://bucket/prefix
--keep-workspace leave build workspaces on disk ($STEPS_KEEP_WORKSPACE)
--answer         answer an ask_user question in advance (repeatable)
--worker         map a step tag to a machine, e.g. --worker gpu=ssh://jt@box
--db             state database: a sqlite path, sqlite:// or postgres:// url
                 (default .steps/steps.db; see Postgres above)
--log-level      debug, info, warn or error (default info, $STEPS_LOG_LEVEL)
```

`--deliver` is listed by `--help` but refused: a daemon receives deliveries at
`POST /p/<pipeline>/hooks/<resource>`, and the flag is for `steps run` and
`steps test`.

`steps pipeline <verb>` — facts about one pipeline:

```
--target         the daemon to talk to (default http://127.0.0.1:8088, $STEPS_TARGET),
                 optionally http://user:password@host
-p / --pipeline  its name on the daemon (set defaults to the YAML's base name)
-c / --config    the YAML to upload (set only)
-v / --var, --vars-file   pipeline vars, substituted before the upload (set only)
--pause          set it paused (set only)
-n / --non-interactive    do not diff or ask; apply it (set, destroy)
--to             the new name (rename only)
```
