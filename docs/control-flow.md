# Control Flow

Three distinct, easy-to-conflate mechanisms for shaping how a job's plan executes, plus the self-verification (`assert:`) that makes fixtures out of them. All are opt-in; a pipeline that uses none of this hashes and behaves exactly as if the feature didn't exist. See `examples/flow.yml` — every job there is a self-verifying, modelless `steps test` fixture; run it with `steps test examples/flow.yml`.

- **`try:`** (tolerate) wraps a step so its failure doesn't stop the plan — best-effort notifications, cleanup, or metrics pushes.
- **`when:`** (guard) runs *before* a step and decides whether it runs at all.
- **`to:`** (route) runs *after* a step and decides which step runs next — including jumping backward to form a loop.
- **Hooks** (`on_success`/`on_failure`/etc.) *react* to a step's outcome with a nested side-step, and never change control flow — you can't build a loop with a hook.

On a failing step with both a hook and a route: the hook fires first (react), then the route reroutes (route).

## Hooks (`on_success`/`on_failure`/`on_error`/`on_abort`/`ensure`)

Any plan step, or the whole job, can carry hooks. A hook is itself a full step (task/put/agent — never `get`), so it can `run:` a command, `put:` a resource, or invoke an `agent:`, and may recursively carry its own hooks.

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
- **Routed-entry only.** A step reached by falling through in declaration order (no `to:` involved) gets no block and no report from `previous_run` — `handoff:` is meaningless there and is rejected at load unless the step is the target of at least one `to:` route within its own get-segment.
- **Agent-only**, and invalid on hook steps (a hook is a reaction, not a positioned step with predecessors).
- A `to.failure` route from a **failed** agent still carries its partial response/trajectory into `previous_run` — the run is packaged from the last attempt's result regardless of outcome. A `to.failure` route from a task/put carries the from-step/key in the block, but `previous_run` reports "no previous run" (there's no agent run to describe).
- A `when:` guard that skips the routed-to step still **consumes** the pending handoff — the transition happened; the guard just declined to run it. The next step to actually execute gets nothing from it.
- **Caching**: only the *declaration* (`context`/`tool` booleans) folds into the step's content hash, value-gated like `image:`/`when:`/`to:` — a step without `handoff:` hashes byte-identically to before this feature existed. The runtime facts a block/tool report (which step routed here, the note, the visit count) are deliberately **excluded** from identity, the same treatment `attempts:` gets — they can't be known at plan time, and agent steps are already unconditionally unskippable, so this never causes a wrong cache hit.

## Assert (self-verification) + `steps test`

`assert:` lets a pipeline verify its own behavior — the mechanism that turns a hooks/control-flow fixture into a runnable regression test. See `examples/flow.yml`, run via `steps test examples/flow.yml`.

- **`assert.execution`**, on a job (ordered task/agent/hook names that must have run) or the pipeline top level (ordered job names). A job's execution is recorded into an in-memory log as the job runs. **A matching job `assert.execution` clears the plan's failure** — evaluated after hooks — so a fixture of deliberately-failing tasks stays green as long as the recorded order matches. A mismatch fails the job with a want/got diff and is never itself cleared. Execution asserts are never hashed (they're meta-checks, like job hooks).
- **`assert.outcome`**, on a job only: `failed` or `succeeded` — what the plan *concluded*, as opposed to which steps ran. `outcome: failed` requires the plan to have produced an error and then clears it (the assertion is what makes the job green); `outcome: succeeded` requires none, and is **not** a no-op — it opts the job out of the clearing rule above, so a plan failure stays a failure. Absent, behavior is exactly as it was before the field existed.

  This exists because `assert.execution` structurally cannot express "this job should have failed." Consider a defect where a failure is swallowed instead of propagated: both builds run the same steps in the same order, so the assert matches either way — and then the match clears the very difference under test. A fixture like that reads as a regression test and is not one. `outcome:` is the observation `execution:` can't make; the two compose, and a mismatch in either fails the job. `examples/flow.yml`'s `failing` job pins it.
- **`assert: {stdout, code}`**, on a task/agent step (`code` is task-only — agents have no exit code). A matching `stdout` substring plus exact `code` makes a non-zero-exit task a *success*. A mismatch is a task-level failure, so `on_failure` fires. When present, assert takes over success determination — a task's `fix:` isn't consulted. This folds into the step's content hash, since it changes the success criteria.
- **`assert.tool_calls`**, on an agent step only: an ordered list of `{name, args}` entries the model's tool calls must satisfy, as an ordered subsequence (extra calls are fine) with subset-matched `args`. Values compare as strings. The trajectory records every call the model requested with its own arguments, *before* any `max_calls:` budget check or `args:` pinning — so a budget-rejected call still appears, and a pinned value is deliberately not matchable (asserting on a pinned key is caught at load time). `stdout` and `tool_calls` are ANDed when both are set.
- **`steps test <pipeline.yml>`** runs every job in declaration order (forced, so the execution log is deterministic), prints per-job PASS/FAIL, and checks the pipeline-level `assert.execution`. This is the self-verifying-fixture entry point.
- There's no modelless agent fixture for `assert.tool_calls` in `examples/` — a `steps test` fixture can't point an agent at a stub, since `source.endpoint:` is a credential boundary and isn't templatable. It's covered instead by unit tests plus the end-to-end tests in `e2e_test.go`, which drive a scripted OpenAI-compatible endpoint (`fakeprovider_test.go`) through a real `run()` and assert on the trajectory, the verdict route, and the recorded outcome. Likewise no `on_error`/`on_abort` fixture yet (would need a docker bad-image task / a per-task `timeout:` directive) — the classification and dispatch machinery already supports both, only the deterministic triggers are missing.

## `in_parallel:` — several steps at once

A plan is otherwise strictly sequential, so independent work waits on itself: three downloads run one at a time, and one slow resource check stalls everything behind it.

```yaml
- in_parallel:
    limit: 2          # max in flight; omit for unbounded
    fail_fast: true   # cancel the siblings on the first failure
    steps:
    - get: repo
    - get: rules
    - task: lint
      run: golangci-lint run
    - task: test
      run: go test ./...
```

`examples/in-parallel.yml` is the runnable version of everything below.

- **The block fails when any branch fails.** `fail_fast:` decides only whether the siblings are cancelled or allowed to finish — never whether the failure counts. That distinction is not hypothetical: the first implementation of this step swallowed a child failure under `fail_fast: false`, and a job containing a failing parallel step reported PASS.
- **Every failure is reported**, not just the first. Debugging a parallel block, the useful question is whether one branch broke or all of them did.
- **Classification follows the worst branch.** An errored branch (infrastructure) outranks a failed one (the step said no), so `on_error` fires rather than `on_failure`.
- **Branches start in declaration order** and, with `limit:`, are admitted in that order. Which branch goes first is otherwise whichever goroutine the scheduler happened to pick, which is nothing a pipeline author can reason about.
- **`assert.execution` stays deterministic.** Branch names are recorded in declaration order, not completion order, so a fixture can name them. The block itself records nothing — it is a container.
- **A branch cannot consume a sibling's output.** Concurrent branches have no order between them, so that would be a race; the plan-time answer is "that artifact is not available here" rather than "sometimes". Everything the branches produce IS available to the steps after the block. Two branches declaring the same output name is a load error, including across nested blocks.
- **The block itself takes no operation fields.** `inputs:`, `outputs:`, `image:`, `run:`, `prompt:`, `trigger:`, `assert:` and friends belong on the step inside it — a block fetches nothing, runs nothing and produces nothing of its own.
- **`limit:` and `fail_fast:` are hashed**, unlike `attempts:`/`timeout:`/`budget:`. They are not "how hard to try": they change which steps run at all, so a cached result from one setting must not satisfy the other.

### Writing a regression test for a parallel block

Use `assert.outcome`. `assert.execution` alone structurally cannot catch a swallowed branch failure — both builds run the same branches, so the assert matches either way and then *clears* the difference under test. `examples/in-parallel.yml`'s `no-fail-fast-still-propagates` job is the shape to copy, and it is verified to go red when the swallow defect is reintroduced.

## `race:` — first success wins

Run a fast/cheap path and a slow/reliable path at the same time, keep whichever finishes successfully first, cancel the other.

```yaml
workspace:
  strategy: copy      # required — see below

jobs:
- name: summarize
  plan:
  - race:
      steps:
      - {agent: fast-model, prompt: "Summarize the release.", outputs: [summary]}
      - {agent: strong-model, prompt: "Summarize the release.", outputs: [summary]}
```

### ⚠️ This costs more, not less

Running both branches **always costs both**, every time, even when the fast one wins. The value is "never wait for the slow path", not "spend less". If you reached for `race:` to save money, you want caching or plain retries instead.

### ⚠️ It is unsafe for side-effecting steps

Cancelling a loser stops only **future** work. A branch that already filed an issue, sent a notification, or pushed a commit keeps that side effect. `race:` is safe for read/generate-only steps; for anything that changes the outside world, run one branch.

Cancellation is also *bounded*, not instantaneous: killing `sh -c "sleep 5; …"` kills the shell but not the `sleep` it forked, so a cancelled step is given a short grace period (see `cancelWaitDelay`) before it is abandoned.

The rest of the rules:

- **"First success" means completed without error** — not a judgment about output quality. A fast but mediocre answer still wins. Gate quality with a downstream `assert:`/`verdicts:` step; folding it in here would make the outcome depend on something no branch can observe about itself.
- **The winner's outputs are the step's outputs**, so a downstream step never has to know which branch won. Every branch must therefore declare the *same* `outputs:` — a mismatch is a load error.
- **Workspace isolation is required**, enforced at load. Losing branches are cancelled while they may be mid-write; under the shared single-directory workspace that lets a loser corrupt the winner's files, and there is no version of that which is safe.
- **A race needs at least two branches.** One runner is a step with extra words.
- **When every branch fails, the block fails** and reports all of them. A hedge is not a guarantee.

## `across:` — one step, once per combination

Run the same step for every combination of some values, instead of writing out a near-identical step per cell and keeping them in sync by hand.

```yaml
- across:
  - var: go_version
    values: ["1.25", "1.26"]
  - var: package
    values: [internal/agent, internal/pipeline]
  task: matrix-test
  image: "golang:{{ .vars.go_version }}"
  run: go test ./{{ .vars.package }}/...
```

`across:` is a **modifier**, not a container: the step it sits on is still a task (or a put, or an agent), it just runs once per cell. `{{ .vars.<name> }}` substitutes into the command, the image, the prompt, the working directory, the step's own name, and each entry of an agent step's `context_paths:` — so a fan-out cell can be *handed* the file it was assigned rather than told to go find it (see [agents.md](agents.md#context_paths-files-delivered-as-synthetic-read_file-results)).

### The headline: per-cell caching

Concourse re-runs the **entire** matrix on any change. Here each cell is hashed and cached individually, so changing one value in one axis re-runs only the cells that value appears in.

That works because cells are **siblings**, parented on the step before the block rather than on the block itself — the block's own hash folds in every cell, so parenting cells under it would make one cell's edit change every cell's identity, which is exactly the whole-matrix re-run this exists to avoid. `examples/across.yml` and `across_test.go` pin it.

Cells that are puts or agents are never skipped, for the same reasons those steps never are anywhere else: side effects and non-determinism.

### The rest

- **Cells run in declaration order, not concurrently** — unless the step says `max_in_flight:` (below). They commonly share a workspace, and a matrix's value is mostly in not hand-maintaining the copies. Put an `in_parallel:` inside a cell if a cell's own work should overlap.
- **A failing cell does not stop the others.** A matrix asks "which of these combinations work", and stopping at the first red one answers that for exactly one cell. Every failure is reported.
- **Cells are named for their coordinates** — `check [mode=fast suite=unit]` — unless you interpolate a variable into the name yourself. Without that every cell would share one name, which is unroutable and unreadable in a log.
- **An empty axis, a duplicate `var:`, or a misspelled `{{ .vars.x }}` are load errors.** Each would otherwise mean silently running the wrong matrix.

### Runtime fan-out: `from:`

An axis can take its values from the **run context** instead of the pipeline text, so an earlier step decides how wide the matrix is:

```yaml
- agent: scanner
  context: write
  prompt: Record the findings worth investigating as a JSON array under `findings`.
- across:
  - var: finding
    from: findings          # instead of values:
  agent: investigator
  prompt: "Investigate: {{ .vars.finding }}"
```

This is "the agent plans, the pipeline executes": one step produces a work list, and each item becomes its own cell — independently hashed, cached and reported — instead of one agent grinding through the whole list in a conversation that outgrows its window.

`values:` and `from:` are mutually exclusive per axis, and an axis needs one of them; both are load errors. A runtime axis can sit beside a static one, and the product is taken as usual.

**The source is a JSON array of strings**, or of flat objects (below). `from:` is the same axis with the list computed later, so a value interpolates through `{{ .vars.x }}` exactly as a static one does and a runtime cell hashes identically to the static cell it is indistinguishable from.

**Because the array is produced during the run, usually by a model, nothing about it was reviewed by the author.** So: at most 1000 items (an unbounded array turns an upstream typo into an unbounded bill — the error says to filter at the source or split the run), a missing key or wrong shape fails the step naming the key, and an empty array is an error rather than a matrix that silently runs nothing.

#### Items with structure

A work item is usually more than a name. A finding has a file, a line, a claim; flattening it to an id means every cell starts by going to look up what it was handed. So a `from:` array may hold **flat objects**, and a cell names the fields it wants:

```yaml
- agent: reviewer
  context: write
  prompt: >
    Record findings under `findings` as a JSON array of flat objects:
    {"id", "file", "line", "claim"}.
- across:
  - var: finding
    from: findings
    label: id            # which field names each cell
  agent: verifier
  prompt: |
    Falsify or confirm: {{ .vars.finding.claim }}
    Evidence lives at {{ .vars.finding.file }}:{{ .vars.finding.line }}.
```

- **Name a field; a bare `{{ .vars.finding }}` is an error.** An object has no single rendering, and choosing one here (JSON? comma-joined?) is the invented rule that kept objects out of `from:` to begin with. Field access has no such problem, because the author names exactly what renders.
- **`label:` says which field names a cell** — `verifier [finding=SQLI-42]`. Coordinates need a scalar: a cell's name is a routing target and an `assert.execution` entry. Without `label:` cells are named by 1-based position (`[finding=#3]`) — deterministic, but it tells a reader nothing. `label:` is invalid on a `values:` axis, where strings already name themselves.
- **Fields must be scalars** (string, number, boolean). Numbers keep their own text, so `"line": 42` renders `42`. A nested object or a list is refused for the same reason a bare object is; record it under its own key, or flatten it where it is written.
- **An array is homogeneous** — all strings or all objects. A mixed one means the step that recorded it disagreed with itself about what an item is, and half the cells would render a template the other half cannot.
- **Every item must carry every field any template names**, and this is checked over the whole array *before any cell runs*. A malformed item fails the block loudly rather than failing cell 7 of 40 after six have already spent their model calls.
- **Nothing about hashing changes**, which is the load-bearing part: a cell's identity is the step it *renders to*, so a field no template mentions never enters it — the same property strings already had.

**Planning.** A static matrix expands at load, so `steps plan` shows every cell. A runtime one cannot: its width is not knowable until the step that fills its source has run. It hashes its *declaration* at plan time — the axes including the source key, plus the unexpanded template — which means the planner cannot predict what that block, or anything downstream of it, will do. The cells themselves hash at run time and cache per cell exactly as static cells do.

### Concurrent cells: `max_in_flight:`

Serial cells are the right default for a hand-written matrix. They are the wrong default for a runtime fan-out, where the cells are N independent agents an earlier step decided on, and running them one at a time costs N times one cell's wall clock for nothing.

```yaml
workspace:
  strategy: copy          # required above 1

- across:
  - var: dimension
    from: dimensions
  max_in_flight: 4        # four cells at a time
  agent: reviewer
  prompt: "Review the diff through the {{ .vars.dimension }} lens."
```

`examples/across-concurrent.yml` is the runnable version, self-verifying via `steps test`.

- **Unset or `1` is the serial walk**, unchanged. This is opt-in, unlike `in_parallel:`'s `limit:` where an absent value means unbounded — each default matches the contract its own block already had. A value at or above the cell count is effectively unbounded.
- **Workspace isolation is required above 1**, enforced at load, for the reason `race:` requires it. A matrix's cells are *clones of one step*: they declare the same `outputs:` and their commands write the same paths, so under the shared strategy two cells at once are two writers on one file — and what survives is neither cell's bytes.
- **Cells are admitted in declaration order.** Under a limit especially, "which cells go first" is otherwise whichever goroutines the scheduler happened to run.
- **`assert.execution` stays deterministic**: each cell records into its own log, merged back in declaration order at the join — the same treatment `in_parallel:` branches get.
- **There is no `fail_fast:`.** A matrix asks which combinations work; cancelling the siblings of the first red cell answers that for exactly one cell. Every cell runs and every failure is reported, exactly as in the serial walk.
- **Recorded context is scoped per cell** and merged at the join under a key naming the cell (`reviewer [dimension=api-boundaries].finding`), because concurrent cells recording one key have no order to resolve by. A *serial* matrix keeps the plain last-wins described in [agents.md](agents.md#writing-from-concurrent-branches) — sequential writers resolve in an order readable off the pipeline. Consequence worth knowing: raising `max_in_flight` changes the key names a downstream step reads.
- **Not hashed**, unlike `in_parallel:`'s `limit:`/`fail_fast:`. Those change which steps run at all; this changes only how many run at once, and the cell set is identical at any width — so widening a matrix whose cells are all cached re-runs nothing.

## `approval:` — a human in the plan

Agent pipelines eventually gate something that cannot be undone — publishing, deploying, sending. Before this there was no place in a plan to stop and ask.

```yaml
- agent: write-draft
  outputs: [draft]
- approval:
    message: "Draft is in draft/summary.md — publish?"
    timeout: 24h
- put: blog
```

```
$ steps approvals pipeline.yml
ID  JOB      REQUESTED             MESSAGE
1   publish  2026-08-05T14:02:11Z  Draft is in draft/summary.md — publish?

$ steps approve pipeline.yml 1
approved: approval 1
```

### Three outcomes, deliberately different

| | classification | why |
|---|---|---|
| approved | the plan continues | |
| rejected | **failed** | a person decided; `on_failure` fires and a `to:` route can act on it |
| expired | **aborted** | nobody decided anything |

Conflating the last two would make a silent expiry indistinguishable from a rejection, and "the deploy was rejected" is a very different thing to read in a log from "the deploy was never looked at".

### ⚠️ v1 scope: anyone who can run the CLI can approve

Stated deliberately rather than left to be discovered — someone will ask the day this ships. There is no separate identity system. The recorded approver comes from `$STEPS_APPROVER`, `$USER`, or `$LOGNAME`: it is an **audit record**, not an authorization check.

### The rest

- **The audit trail is a row, not a chat message.** Who approved, when, and why a rejection was a rejection are exactly the facts someone needs to reconstruct a decision later, and they must not depend on external chat history.
- **A decision cannot be overwritten.** Deciding an already-decided approval is an error; the record is a record.
- **Nothing about an approval is cacheable.** A cached "they said yes once" must never stand in for asking again.
- **Notification is the missing half, and it is not built in.** A parked approval nobody is told about is useless in practice. Today the step prints the message and the exact command to answer it, and logs `job.approval_pending` — put a `put:` to Slack or email in the step *before* the approval to actually reach someone. When the circuit breaker's notification path lands, both should use it.
