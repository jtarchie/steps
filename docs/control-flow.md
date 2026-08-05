# Control Flow

Four distinct, easy-to-conflate mechanisms for shaping how a job's plan executes, plus the self-verification (`assert:`) that makes fixtures out of them. All are opt-in; a pipeline that uses none of this hashes and behaves exactly as if the feature didn't exist. See `examples/flow.yml` — every job there is a self-verifying, modelless `steps test` fixture; run it with `steps test examples/flow.yml`.

- **`try:`** (tolerate) wraps a step so its failure doesn't stop the plan — best-effort notifications, cleanup, or metrics pushes.
- **`in_parallel:`** (concurrency) runs several steps at once with an optional limit and on-first-failure cancellation.
- **`when:`** (guard) runs *before* a step and decides whether it runs at all.
- **`to:`** (route) runs *after* a step and decides which step runs next — including jumping backward to form a loop.
- **Hooks** (`on_success`/`on_failure`/etc.) *react* to a step's outcome with a nested side-step, and never change control flow — you can't build a loop with a hook.

On a failing step with both a hook and a route: the hook fires first (react), then the route reroutes (route).

## Hooks (`on_success`/`on_failure`/`on_error`/`on_abort`/`ensure`)

Any plan step, or the whole job, can carry hooks. A hook is itself a full step (task/put/agent/try/in_parallel — never `get`), so it can `run:` a command, `put:` a resource, invoke an `agent:`, run several steps concurrently, or wrap a tolerated step, and may recursively carry its own hooks.

```yaml
jobs:
- name: build
  plan:
  - task: work
    run: ./build.sh
    on_failure:                # step-level hook
      task: notify
      run: ./notify.sh failed
    ensure:
      task: cleanup
      run: ./cleanup.sh
  on_success:                  # job-level hook (inline alongside plan:)
    put: status
```

- **Hooks observe, they don't consume.** A failing step's `on_failure` runs, and the failure still propagates — the job fails, `steps run` exits nonzero. This is the opposite of "swallow the error": a hook never clears a failure (only a matching `assert.execution` does that — see below). The one way a hook changes an outcome is upward: a failing `on_success` or `ensure` fails an otherwise-green step/job, since a broken notification or cleanup shouldn't read as success. A failing `on_failure`/`on_error`/`on_abort` is only logged.
- **Classification** buckets every step error against the job context: **failed** (a task-level failure — nonzero exit, a red fix verdict, a required tool that never succeeded), **errored** (everything else unmarked — workspace setup, docker, LLM transport, a resource check), or **aborted** (`ctx.Err() != nil`, i.e. SIGINT/SIGTERM mid-run). A docker-level exit (125/126/127) classifies as *failed*, since it's indistinguishable from the command's own exit code.
- **Ordering**: the single matching `on_*` hook runs, then `ensure` always runs last. Hooks reached after cancellation run under a 60-second grace period detached from the canceled context, so they complete but not forever.
- **No identity of their own**: a hook step records no cache node and no `job_run` — the enclosing step/job records the aggregate outcome.
- **Step hooks fold into the step's content hash**: editing a hook — or the top-level `tasks:`/`agents:` entry it references — busts the parent step's cache. A skipped (cached) step fires no hooks, since it didn't run.
- **Job hooks are never hashed** and fire on every `RunJob` invocation, even a fully-cached one, since they don't alter what any step executes. A job-level hook may not declare `inputs:`/`outputs:` (it runs in an empty per-build workspace); step-level hooks keep full support.

## Conditional steps (`when:`)

A `task`/`put`/`agent` step (including a hook step) can carry `when:` — a shell command whose **exit code** decides whether the step runs: 0 runs it, nonzero skips it.

```yaml
- task: scout
  run: ./scout.sh > risk.txt
- task: deep-review          # only runs when the cheap step says so
  run: ./review.sh
  when: grep -q high risk.txt          # scalar shorthand
- put: report
  when: { run: test -s findings.txt }  # mapping form
```

- **The exit code is the whole contract.** A nonzero exit is a legitimate *false* (`grep -q` matching nothing, `test -f` on a missing file) and never a failure. Only a runner-level error — the command couldn't even be started (bad cwd, bad image, dead docker daemon) — fails the step. Note a shell "command not found" is exit 127, i.e. a false guard, not an error.
- **A guard-skipped step skips only itself; the plan continues.** This is different from a cache cache hit, which stops the whole remaining chain.
- A skipped step fires no hooks and records no cache node, `job_run`, or execution-log entry — the same contract as a cached skip. That's what lets `assert.execution` prove a step was skipped.
- The guard runs under the step's own resolved image, in a workspace materialized from the step's declared `inputs:`, but closed without capturing outputs — a guard can never publish artifacts.
- The guard command is folded into the step's content hash, but its *outcome* is a run-time fact the planner can't know, so any chain containing a `when:` step is unskippable — never recorded as a reusable "this whole chain succeeded" hash.
- Invalid on `get` steps (a get fans the remainder of the plan out per version, so a conditional get has no coherent meaning).
- This is how an agent decides routing without typed outputs: an `agent` step writes a verdict into a declared output artifact, and the next step's `when:` tests that file. The model proposes; a deterministic command disposes; both are visible in the YAML.

## Tolerated failure (`try:`)

A `try:` wraps a single inner step (task, put, agent, or another try) so a **task-level failure** of that step doesn't stop the plan. Every job in `examples/try.yml` is a modelless, self-verifying fixture for the rules below — run it with `steps test examples/try.yml`.

```yaml
- try:
    put: slack-notify
    params:
      text: "build finished"
```

The wrapper is **transparent**: the only thing it changes is whether the plan walker stops. Everything that *observes* the outcome sees the truth.

- **The wrapped step runs exactly as it would unwrapped.** Its `when:` guard decides whether it runs at all (a guard-skipped step records no execution, wrapper or not), its own hooks fire on its real outcome, and its node records that outcome. The wrapper records a second node, `succeeded` when the failure was tolerated.
- **Only `outcome.Failed` is tolerated.** An infrastructure error (docker, transport, workspace) or an abort (Ctrl-C) still stops the run and exits non-zero — the same line `to:` routing draws. Tolerating those would report a green job for a canceled build and march the plan into steps whose context is already dead.
- **`to:` routing sees the real outcome**, because toleration happens *after* routing. `to: {failure: cleanup}` on the wrapper is reachable. The target name is the wrapped step's own name (task/put/agent).
- **Hooks on the wrapper** also observe the real outcome, so `on_failure` on a `try:` fires when the wrapped step failed.
- **The wrapper is the plan-positioned step**, so `to:` and `max_visits:` belong on it and are rejected on the step it wraps (where they used to load fine and silently never fire). `verdicts:`, `handoff:` and `handoff_note:` stay on the `agent:` step being wrapped, since that is what the agent runtime reads — a tolerated agent still routes on its verdict and still receives its transition context.
- **`assert:` is rejected anywhere inside a `try:`**, on the wrapper and on the step it wraps alike. `assert:` is what makes a step a `steps test` fixture and `try:` swallows exactly the failure it reports, so such an assert could never fail a run — it would sit in a suite reporting PASS on the broken fixture it was written to catch.
- **Composability:** `try:` nests (doubled `try:` is fine) and composes with `attempts:`/`timeout:` on the wrapped step — retry a few times, then shrug. Also works with `fix:` on a task — attempt repair, then tolerate if the fix doesn't stick.
- **Artifacts flow through unchanged**: a wrapped task's `outputs:` are available to later steps exactly as if it were unwrapped (note that a *tolerated* step may not have produced them — a later `inputs:` on that artifact is a static contract, not a runtime guarantee).
- **Output visibility**: when a failure is tolerated, the run prints `try: <name> failed (tried, continuing)` so the transcript doesn't read all-green while quietly eating a failure.
- **Invalid on get steps** (a get fans the remainder of the plan per version, which has no coherent meaning inside a tolerated wrapper). Try wrapping get is rejected at load time.
- **Valid as a hook body**: `ensure: { try: { put: slack-notify } }` is the usual home for best-effort notification — a failing `on_success`/`ensure` hook otherwise fails an otherwise-green step, and the wrapper is what stops that.
- **Always unskippable**: the try wrapper and everything downstream of it always executes — removing `try:` from a step changes its identity, so re-running after an edit must not read a stale cache.

## Concurrency (`in_parallel:`)

`in_parallel:` wraps several child steps and runs them concurrently instead of one after another. Each child runs its own hooks, records its own outcome, and participates in artifact flow like any other step. The wrapper occupies a single position in the plan and aggregates outcomes. Every `examples/in-parallel-*.yml` fixture is a modelless, self-verifying `steps test` example.

```yaml
- in_parallel:
    limit: 2          # max in flight; unset or 0 means unbounded
    fail_fast: true   # first failure cancels the rest (default: false)
    steps:
    - task: lint
      run: echo "lint complete"
    - task: test
      run: echo "test complete"
```

- **`limit:`** bounds how many children run at once. Unset or 0 means every child starts immediately (unbounded). This is the tool for bounding container/CPU/memory load when several parallel steps each set `image:`.
- **`fail_fast:`** controls sibling cancellation. When `true` (the default is `false`), the first child failure stops the remaining children — their contexts are cancelled and they receive `[branch N] failed: context canceled`. When `false`, all children run to completion regardless of sibling failures — but the wrapper still returns the error and the job fails (it is NOT a tolerance mechanism like `try:`).
- **Aggregation**: on `fail_fast: true`, the first child error becomes the wrapper's error. On `fail_fast: false`, the first error encountered is returned — all children still run, but the aggregate is still an error (the wrapper does *not* swallow failures the way `try:` does).

### What is refused

`in_parallel:` is a container, not a step that itself does work. Several fields that belong on individual children are rejected on the wrapper at load time:

- **`when:`**, **`image:`**, **`assert:`**, **`trigger:`**, **`version:`**, **`params:`**, **`resource:`** on an `in_parallel:` wrapper are all rejected — set them on the individual children that need them.
- **Cross-branch routing** — a child's `to:` or `verdicts:` target must resolve within the same branch. Routing to a sibling branch or outside the block is rejected at load. See Routing scope below.
- **Duplicate output names** across children are rejected at load — two children at any nesting depth both producing an artifact named `result` is a naming collision. Duplicates are detected recursively through nested `in_parallel:` blocks. Silent last-write-wins would be a correctness trap, so it's a hard error instead.
- **`get` steps as children** are not supported. A get step fans the remainder of the plan out per version, which has no coherent meaning inside a concurrent block. Get steps inside `in_parallel:` are rejected at load time. Put your `get` steps outside the `in_parallel:` block.

### Routing scope

A child step's `to:` (or an agent child's `verdicts:`) may route to another child **within the same branch**. Cross-branch routing — a child in branch 0 routing to a step in branch 1 — is rejected at load. Routing to a step outside the `in_parallel:` block entirely is also rejected. Each branch is its own routing mini-segment.

Within-branch routing targets are validated at load time but **not followed at runtime** — every child runs concurrently regardless of any child's `to:` disposition. This is a consequence of concurrent execution: all children always run, so there is nothing to route around. The `examples/in-parallel-routing.yml` fixture passes because all three children run concurrently anyway, not because the `to:` route is executed.

A consequence of routes not being followed: **`handoff:` and `handoff_note:` on children inside `in_parallel:`** are validated at load time (the route target is checked to exist within the same branch) but are never delivered at runtime — the handoff machinery fires on entry from a `to:` route, and no route is ever followed to a child inside `in_parallel:`. Declaring either inside a concurrent block is harmless but has no effect. This also means a `previous_run` handoff tool inside an in_parallel branch will always answer `\"no previous run\"`.

`to:`/`max_visits:` on the wrapper itself controls how the wrapper routes *out* of the block after aggregation — the same contract as any other step with `to:`/`max_visits:`. Other steps cannot route *to* an in_parallel wrapper — it has no routable name — so a backward route from outside into the block is impossible.

### Nesting

`in_parallel:` inside `in_parallel:` works from day one. Children at any nesting depth execute concurrently within their own branch scope. The `examples/in-parallel-nested.yml` fixture verifies two levels and the same rules (limit, fail_fast, routing scope, duplicate-output rejection) apply recursively.

### Hooks

Hooks (`on_success`/`on_failure`/`on_error`/`on_abort`/`ensure`) on the `in_parallel:` wrapper fire after all children complete, on the aggregated outcome. Children run their own hooks independently. `in_parallel:` is also valid as a hook body — `ensure: { in_parallel: { ... } }` is the way to run two cleanup steps in parallel.

### Execution log and caching

- **Execution log**: children record themselves in **declaration order** after all complete, not in the order they finish — so `assert.execution` on a job that contains `in_parallel:` is deterministic regardless of which child's goroutine finished first. The wrapper itself records nothing.
- **Always unskippable**: the in_parallel wrapper and everything downstream of it always executes — concurrent execution is non-deterministic by nature. Removing `in_parallel:` from a step changes its identity, so re-running after an edit must not read a stale cache.
- **Output labeling**: each child's output lines are prefixed with `[branch N]` so concurrent interleaving is readable. For example a three-child block prints `[branch 0] starting`, `[branch 1] starting`, `[branch 2] starting`, then each child's stdout/stderr and `[branch N] done` or `[branch N] failed: ...`.

### Workspace safety

All children of an `in_parallel:` block share one workspace by default. Two concurrent tasks writing to the same file path is a data race and can silently corrupt output. If your children write files, either coordinate them (each child writes to a distinct subdirectory) or switch to isolated workspaces — see [workspace.md](workspace.md) for `strategy: copy` (safe on any platform) and `strategy: btrfs` (fast copy-on-write snapshots, Linux only). This hazard is not detected at load time — `in_parallel:` does not require workspace isolation, but the docs must tell you before you hit it.

## Step transitions (`to:`/`max_visits:`/`verdicts:`)

A `task`/`put`/`agent` step can carry `to:` — a map that routes to another step **in the same get-segment** based on this step's outcome, including jumping backward to form a bounded loop (a judge/revise cycle). Task loops are self-verifying via `examples/flow.yml`; verdict routing is illustrated in `examples/agents.yml` (needs a model).

- **Routing keys**: `success`/`failure` for a task/put/verdict-less agent, or a verdict name for a verdict agent (below). An errored or aborted step produces no key and never routes — it propagates, so a loop can't spin during shutdown or mask an outage. A `to.failure` route consumes the failure (the job doesn't also fail).
- **Agent verdict routing (`verdicts:`)** is the N-way judge. An agent step declaring `verdicts: [approve, revise, escalate]` gets a synthesized *required* `verdict` tool whose enum is exactly those verdicts — the model must call it, reusing the required-tool `tool_choice` forcing from [agents.md](agents.md) verbatim. The choice becomes the routing key. Every declared verdict must have a `to:` target; `failure:` is reserved for "never emitted a verdict / errored." There's no generic `success` key in verdict mode, and verdict mode is agent-only.
- **`max_visits:`** caps how many times a step executes (per `runSteps` invocation, so `version: every` gives each version its own budget), and is required at load time for any step whose `to:` routes backward. Exceeding it is a job-level failure, reaching the job's own `on_failure`/`ensure` — distinct from the step's own per-iteration `on_failure`.
- **Get-segment restriction**: a `to:` target must be within the same segment (the run of non-get steps between gets) — a jump can't cross a `get` anyway, since the plan re-enters over a truncated slice per version. `to:`/`verdicts:` are invalid on `get` steps and hook steps. Step names must be unique within a `to:`-using segment.
- **Caching**: `to:`/`max_visits:`/`verdicts:` fold into the step's content hash; `verdicts:` matters because it changes the synthesized tool set. There's no structural cache change — the planner walks declaration order ignoring `to:` targets, because any routing step's chain is already unskippable, so its plan-time hash is never used for a skip.

## Handoff context (`handoff:`)

Every agent step is otherwise a fresh, hermetic conversation — a step reached via `to:`/`verdicts:` learns nothing about why it was invoked. `handoff:` opts an agent step into transition context on **routed entry only**: the step's first/unrouted execution is unaffected. Illustrated in `examples/agents.yml`'s `judge` job (needs a model).

> `handoff:` has two directions, and each is a field rather than a separate key. `context:`/`tool:` look **backward**, along a route, for a step being sent back to redo something. `note:` looks **forward** — what this agent hands the next one on the normal path; see [authored handoff](agents.md#authored-handoff-notes) in the agents guide. Both compose on one step: `handoff: { tool: true, note: true }`.
>
> In the mapping form every field defaults to off and means only itself. Only `context:`/`tool:` require a `to:` route to target this step; a note-only handoff needs none.

```yaml
- agent: writer
  prompt: Draft a two-sentence summary...
  handoff: true              # scalar shorthand: context block only
# or
  handoff: { tool: true }             # + a previous_run tool
  handoff: { context: false, tool: true }  # tool only, no pushed block
```

- **Push (`context`, default `true` in the mapping form)**: when a `to:` transition lands on the step, a machine-assembled block is appended to its prompt (never mutating `prompt:` itself, in the same spirit as the fix agent's captured-failure-output idiom — see [agents.md](agents.md)):

  ```
  <transition_context>
  entered via: revise (from step "critic")
  visit: 2 of 3 for this step
  position: step 1 of 4 in job "judge"
  <note from="critic">
  The second sentence overstates test coverage; tighten it.
  </note>
  </transition_context>
  ```

  The `<note>` element is present only when the routing step was a verdict agent that gave one (see `verdicts:`'s optional `note` arg in [agents.md](agents.md)) — a deliberate, authored "why," not a summary. `visit:` reads `(unbounded)` when the target has no `max_visits:` (an all-forward route). A note's content is truncated and sanitized (a literal `</note>` can't close the element early) — the same trust domain as any other upstream model-authored text.
- **Pull (`tool`)**: synthesizes a read-only, non-required `previous_run` tool the model can call on demand, returning the routed-from **agent** step's recorded run — final response, verdict + note, turn count, and tool-call trajectory (optionally filtered to `section: response` or `section: trajectory`) — without any of it entering context unrequested. When there's nothing to report (first execution, or the routing step wasn't an agent), it answers `"no previous run: ..."` as data, never an error.
- **Routed-entry only.** A step reached by falling through in declaration order (no `to:` involved) gets no block and no report from `previous_run` — `handoff:` is meaningless there and is rejected at load unless the step is the target of at least one `to:` route within its own get-segment. `handoff:` on a child inside an `in_parallel:` block is validated at load but never delivered at runtime — routes within `in_parallel:` are not followed (see [Concurrency](#concurrency-in_parallel)).
- **Agent-only**, and invalid on hook steps (a hook is a reaction, not a positioned step with predecessors).
- A `to.failure` route from a **failed** agent still carries its partial response/trajectory into `previous_run` — the run is packaged from the last attempt's result regardless of outcome. A `to.failure` route from a task/put carries the from-step/key in the block, but `previous_run` reports "no previous run" (there's no agent run to describe).
- A `when:` guard that skips the routed-to step still **consumes** the pending handoff — the transition happened; the guard just declined to run it. The next step to actually execute gets nothing from it.
- **Caching**: only the *declaration* (`context`/`tool` booleans) folds into the step's content hash, value-gated like `image:`/`when:`/`to:` — a step without `handoff:` hashes byte-identically to before this feature existed. The runtime facts a block/tool report (which step routed here, the note, the visit count) are deliberately **excluded** from identity, the same treatment `attempts:` gets — they can't be known at plan time, and agent steps are already unconditionally unskippable, so this never causes a wrong cache hit.

## Assert (self-verification) + `steps test`

`assert:` lets a pipeline verify its own behavior — the mechanism that turns a hooks/control-flow fixture into a runnable regression test. See `examples/flow.yml`, run via `steps test examples/flow.yml`.

- **`assert.execution`**, on a job (ordered task/agent/hook names that must have run) or the pipeline top level (ordered job names). A job's execution is recorded into an in-memory log as the job runs. **A matching job `assert.execution` clears the plan's failure** — evaluated after hooks — so a fixture of deliberately-failing tasks stays green as long as the recorded order matches. A mismatch fails the job with a want/got diff and is never itself cleared. Execution asserts are never hashed (they're meta-checks, like job hooks).
- **`assert: {stdout, code}`**, on a task/agent step (`code` is task-only — agents have no exit code). A matching `stdout` substring plus exact `code` makes a non-zero-exit task a *success*. A mismatch is a task-level failure, so `on_failure` fires. When present, assert takes over success determination — a task's `fix:` isn't consulted. This folds into the step's content hash, since it changes the success criteria.
- **`assert.tool_calls`**, on an agent step only: an ordered list of `{name, args}` entries the model's tool calls must satisfy, as an ordered subsequence (extra calls are fine) with subset-matched `args`. Values compare as strings. The trajectory records every call the model requested with its own arguments, *before* any `max_calls:` budget check or `args:` pinning — so a budget-rejected call still appears, and a pinned value is deliberately not matchable (asserting on a pinned key is caught at load time). `stdout` and `tool_calls` are ANDed when both are set.
- **`steps test <pipeline.yml>`** runs every job in declaration order (forced, so the execution log is deterministic), prints per-job PASS/FAIL, and checks the pipeline-level `assert.execution`. This is the self-verifying-fixture entry point.
- There's no modelless agent fixture for `assert.tool_calls` in `examples/` — a `steps test` fixture can't point an agent at a stub, since `source.endpoint:` is a credential boundary and isn't templatable. It's covered instead by unit tests plus the end-to-end tests in `e2e_test.go`, which drive a scripted OpenAI-compatible endpoint (`fakeprovider_test.go`) through a real `run()` and assert on the trajectory, the verdict route, and the recorded outcome. Likewise no `on_error`/`on_abort` fixture yet (would need a docker bad-image task / a per-task `timeout:` directive) — the classification and dispatch machinery already supports both, only the deterministic triggers are missing.
