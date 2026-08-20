# The web UI

```bash
steps web pipeline.yml
```

Serves a browser view of what the runner has done and is doing, at
`http://127.0.0.1:8088`. Several pipelines at once:

```bash
steps web app.yml infra.yml nightly.yml
```

Each is routed under `/p/<basename>/`, because state is per-pipeline by
construction — `.steps/state.db` lives beside each YAML, so two pipelines
never share a database here either.

## What it shows

| Route | Answers |
|---|---|
| `/p/:pipeline` | Which jobs exist, how each last run went, and which jobs feed which — list or `git log`-style graph |
| `…/jobs/:job` | This job's dependencies in both directions, its run history with a duration trend, and the resource versions it has passed against |
| `…/runs/:run` | **The transcript**: every step in plan order, what it did, and — for agent steps — what the model said and which tools it called |
| `…/nodes/:hash` | What a merkle hash is made of, and every run that reused it: the cache's receipt |
| `…/approvals` | Pending `approval:` steps, and the decisions already made |
| `…/resources` | Latest checked version per resource, and any job the circuit breaker has paused |
| `/docs` | These docs, rendered with syntax-highlighted examples — the same pages `steps docs` shows in a terminal |

Press `/` anywhere for a jump palette over pipelines, jobs, and recent runs.

## The transcript

The run page is the point of the whole thing. It renders a run the way the
terminal does — steps in order, prefixed by kind, colored by outcome — with
the things a scrollback cannot give you:

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
- **A CLI-backed agent shows its tool calls too.** An agent whose
  `source.model:` is a CLI (`@claude/sonnet`) runs its own tool loop in a
  subprocess and publishes no turns, but the calls it made are recorded with
  the step and are listed under it.
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

Three controls, each writing the same rows the CLI writes:

- **Trigger** / **Re-run (forced)** enqueue the job into the durable trigger
  queue `steps watch` uses. `steps web` drains that queue in-process by
  calling `pipeline.RunJob` — there is no second execution path, so a job run
  from a browser gets the same caching, hooks, serial groups, and recording as
  any other. Forced re-run skips the merkle cache; an unforced one does not,
  which on an unchanged pipeline correctly does almost nothing.
- **Approve / Reject** on an `approval:` step, with the reason recorded — the
  same row `steps approve` writes.
- **Resume** a job the watch circuit breaker paused.

`--read-only` withholds all three: the controls disappear from the pages and
the routes refuse. The queue is still drained, since a row queued by a
separate `steps watch` is work this process can still do.

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
--read-only      serve without trigger, approval, or resume controls
--keep-workspace leave build workspaces on disk
--var / --vars-file   pipeline vars, as everywhere else
```
