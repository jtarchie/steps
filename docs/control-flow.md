# Control Flow

Four distinct, easy-to-conflate mechanisms for shaping how a job's plan executes, plus the self-verification (`assert:`) that makes fixtures out of them. All are opt-in; a pipeline that uses none of this hashes and behaves exactly as if the feature didn't exist.

- **`do:`** (group) runs several steps as one, so a single hook covers the whole group.
- **`try:`** (tolerate) wraps a step so its failure does not stop the plan — best-effort notifications, cleanup, or metrics pushes.
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
    run: echo building
    on_success:                # step-level hook
      task: record
      run: echo built cleanly
    ensure:
      task: cleanup            # runs whatever happened above
      run: echo tidying up
  on_success:                  # job-level hook (inline alongside plan:)
    task: announce
    run: echo the whole job passed
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
jobs:
- name: guarded
  plan:
  - task: scout
    outputs: [risk]
    run: echo low > risk/level.txt
  - task: deep-review                       # skipped: the guard exits 1
    inputs: [risk]
    when: grep -q high risk/level.txt       # scalar shorthand
    run: echo auditing everything
  - task: report                            # runs: the guard exits 0
    inputs: [risk]
    when: { run: test -s risk/level.txt }   # mapping form
    run: cat risk/level.txt
```

- **The exit code is the whole contract.** A nonzero exit is a legitimate *false* (`grep -q` matching nothing, `test -f` on a missing file) and never a failure. Only a runner-level error — the command couldn't even be started (bad cwd, bad image, dead docker daemon) — fails the step. Note a shell "command not found" is exit 127, i.e. a false guard, not an error.
- **A guard-skipped step skips only itself; the plan continues.** This is different from a cache hit, which stops the whole remaining chain.
- A skipped step fires no hooks and records no cache node, `job_run`, or execution-log entry — the same contract as a cached skip. That's what lets `assert.execution` prove a step was skipped.
- The guard runs under the step's own resolved image, in a workspace materialized from the step's declared `inputs:`, but closed without capturing outputs — a guard can never publish artifacts.
- The guard command is folded into the step's content hash, but its *outcome* is a run-time fact the planner can't know, so any chain containing a `when:` step is unskippable — never recorded as a reusable "this whole chain succeeded" hash.
- Invalid on `get` steps (a get fans the remainder of the plan out per version, so a conditional get has no coherent meaning).
- This is how an agent decides routing without typed outputs: an `agent` step writes a verdict into a declared output artifact, and the next step's `when:` tests that file. The model proposes; a deterministic command disposes; both are visible in the YAML.

## Tolerated failure (`try:`)

A `try:` wraps a single inner step (task, put, agent, or another try) so a **task-level failure** of that step doesn't stop the plan:

```yaml
jobs:
- name: ship
  plan:
  - task: build
    run: echo ok
  - try:
      task: notify           # fails — and the plan shrugs
      run: "false"
  - task: after
    run: echo still running
```

The wrapper is **transparent**: the only thing it changes is whether the plan walker stops. Everything that *observes* the outcome sees the truth.

- **The wrapped step runs exactly as it would unwrapped.** Its `when:` guard decides whether it runs at all, its own hooks fire on its real outcome, and its node records that outcome. The wrapper records a second node, `succeeded` when the failure was tolerated.
- **Only a task-level failure is tolerated.** An infrastructure error (docker, transport, workspace) or an abort (Ctrl-C) still stops the run and exits non-zero — the same line `to:` routing draws. Tolerating those would report a green job for a canceled build and march the plan into steps whose context is already dead.
- **`to:` routing sees the real outcome**, because toleration happens *after* routing. `to: {failure: cleanup}` on the wrapper is reachable. The target name is the wrapped step's own name (task/put/agent).
- **Hooks on the wrapper** also observe the real outcome, so `on_failure` on a `try:` fires when the wrapped step failed.
- **The wrapper is the plan-positioned step**, so `to:` and `max_visits:` belong on it and are rejected on the step it wraps (where they used to load fine and silently never fire). `verdicts:` (targets and all) stays on the `agent:` step being wrapped, since that is what the agent runtime reads — a tolerated agent still routes on its verdict.
- **`assert:` is rejected anywhere inside a `try:`**, on the wrapper and on the step it wraps alike. `assert:` is what makes a step a `steps test` fixture and `try:` swallows exactly the failure it reports, so such an assert could never fail a run.
- **Composability:** `try:` nests and composes with `attempts:`/`timeout:` on the wrapped step — retry a few times, then shrug. Also works with `fix:` on a task — attempt repair, then tolerate if the fix doesn't stick.
- **Artifacts flow through unchanged**: a wrapped task's `outputs:` are available to later steps exactly as if it were unwrapped (note that a *tolerated* step may not have produced them — a later `inputs:` on that artifact is a static contract, not a runtime guarantee).
- **Output visibility**: when a failure is tolerated, the run prints `try: <name> failed (tried, continuing)` so the transcript doesn't read all-green while quietly eating a failure.
- **Invalid on get steps**, rejected at load time.
- **Valid as a hook body**: `ensure: { try: { put: slack-notify } }` is the usual home for best-effort notification — a failing `on_success`/`ensure` hook otherwise fails an otherwise-green step, and the wrapper is what stops that.
- **Always unskippable**: the try wrapper and everything downstream of it always executes — removing `try:` from a step changes its identity, so re-running after an edit must not read a stale cache.

## Step transitions (`to:`/`max_visits:`/`verdicts:`)

A step routes to another step **in the same get-segment** based on its outcome, including jumping backward to form a bounded loop. Two spellings, one per kind of outcome: a `task`/`put`/verdict-less `agent` carries `to:`, a binary `success`/`failure` map; an `agent` carries `verdicts:`, an ordered list that declares its vocabulary **and** its targets together.

A task loop, modelless and complete — `bump` advances state (and always succeeds, so its write is captured), `check` decides and routes backward until it's satisfied:

```yaml
jobs:
- name: converge
  plan:
  - task: seed
    outputs: [state]
    run: touch state/visits.txt
  - task: bump
    inputs: [state]
    outputs: [state]                # read-modify-write: each visit continues the last
    run: echo visit >> state/visits.txt
  - task: check
    inputs: [state]
    run: test "$(wc -l < state/visits.txt)" -ge 3
    to: { failure: bump }           # backward — so max_visits is required
    max_visits: 5
  - task: done
    run: echo converged
```

And verdict routing — the agent's decision picks the next step:

```yaml test=control-verdicts
agents:
- name: critic
  source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
- name: review
  plan:
  - task: draft
    outputs: [notes]
    run: echo 'first draft' > notes/draft.txt
  - agent: critic
    inputs: [notes]
    prompt: "Read notes/draft.txt. Approve it, or send it back."
    verdicts:
      - approve: publish       # route: record the verdict and jump forward
      - revise: draft          # backward — the loop max_visits: bounds
      - failure: escalate      # reserved: the step errored or decided nothing
    max_visits: 3
  - task: escalate
    run: echo paging a human
  - task: publish
    run: echo publishing
```

- **Routing keys**: `success`/`failure` for a task/put/verdict-less agent, or a verdict name for a verdict agent. An errored or aborted step produces no key and never routes — it propagates, so a loop can't spin during shutdown or mask an outage. A `to.failure` route consumes the failure (the job doesn't also fail).
- **Agent verdicts (`verdicts:`)** is the N-way judge. A step declaring verdicts gets a synthesized *required* `verdict` tool whose enum is exactly the declared names in declaration order — the model must call it (see [agents.md](agents.md)). The choice becomes the routing key. Agent-only (an `ensemble:` counts — its members vote and the block routes on the decision).
  - **An entry is a name, or a `name: target` pair.** Bare means *record the verdict and fall through in declaration order* — a classifier whose product is the decision needs no targets at all, and gets the forced tool anyway. `- approve: next` is the same thing said explicitly.
  - **The order is load-bearing** twice over: it is the enum the model is offered, and it is `decide: any`'s precedence for an ensemble. That is why targets live in this list rather than a `to:` map beside it — a YAML map has no order, and the two lists had to be cross-checked key for key to catch a typo in either.
  - **`failure:` is reserved** for "the step errored or never emitted a verdict". It is excluded from the enum (the model cannot choose it; the runtime arrives at it) and it must name a target — bare `failure` would mean "tolerate the failure and carry on", which is `try:`'s job. `success` is not a verdict name: a verdict already *is* the step's success.
  - **`to:` and `verdicts:` are mutually exclusive.** A step carrying both is refused at load, with the new form in the message.
- **`next`** is the one target that is not a step name: *continue in declaration order*. It exists because a verdict must name a target and a **container has none** — `in_parallel:`, `race:`, `across:`, `ensemble:` and `approval:` have no name to be jumped to, so without it an author whose next step is one of those has to route *past* it. That is not equivalent, and quietly so: routing past an `approval:` skips the human gate the pipeline exists to have. `next` is always forward, so it never requires `max_visits:`, and it is a real route rather than a fall-through — `failure: next` **consumes** the failure, which is how a step says "carry on anyway":

```yaml
jobs:
- name: fall-through
  plan:
  - task: flaky
    run: "false"
    to: { failure: next }    # the failure is consumed; the job stays green
  - task: report
    run: echo carrying on
```

  The word is reserved on the value side: a step actually named `next` inside a `to:`-using segment is a load error naming the collision.
- **`max_visits:`** caps how many times a step executes (per plan walk, so `version: every` gives each version its own budget), and is required at load time for any step whose `to:` routes backward. Exceeding it is a job-level failure, reaching the job's own `on_failure`/`ensure` — distinct from the step's own per-iteration `on_failure`.
- **Get-segment restriction**: a target must be within the same segment (the run of non-get steps between gets) — a jump can't cross a `get` anyway, since the plan re-enters over a truncated slice per version. `to:`/`verdicts:` are invalid on `get` steps and hook steps. Step names must be unique within a `to:`-using segment. `next` on the segment's **last** step resolves one past the end, which is where an unrouted final step goes anyway.
- **Caching**: `to:`/`max_visits:`/`verdicts:` fold into the step's content hash; `verdicts:` matters because it changes the synthesized tool set, and its entries hash as an ordered array so a reorder AND a retarget each bust it. There's no structural cache change — the planner walks declaration order ignoring `to:` targets, because any routing step's chain is already unskippable.

> **Consuming a verdict without routing on it.** A verdict is also readable downstream: a later step declaring `context: { from: { <step>: verdict|note|full } }` is handed what that step decided — as a synthetic `read_step` result for an agent, as a file for a task. It needs no route between the two steps, which is what makes a bare-verdict classifier's decision usable. See [agents.md](agents.md).

## Assert (self-verification) + `steps test`

`assert:` lets a pipeline verify its own behavior — the mechanism that turns a hooks/control-flow fixture into a runnable regression test. This whole fixture *deliberately fails* and stays green, because the assertions say the failure is the point:

```yaml
jobs:
- name: failing-fixture
  plan:
  - task: boom
    run: "false"
    on_failure:
      task: alarm
      run: echo the failure was observed
  assert:
    execution: [boom, alarm]    # these ran, in this order — and that clears the failure
    outcome: failed             # and the plan really did fail
```

- **`assert.execution`**, on a job (ordered task/agent/hook names that must have run) or the pipeline top level (ordered job names). **A matching job `assert.execution` clears the plan's failure** — evaluated after hooks — so a fixture of deliberately-failing tasks stays green as long as the recorded order matches. A mismatch fails the job with a want/got diff and is never itself cleared. Execution asserts are never hashed (they're meta-checks, like job hooks).
- **`assert.outcome`**, on a job only: `failed` or `succeeded` — what the plan *concluded*, as opposed to which steps ran. `outcome: failed` requires the plan to have produced an error and then clears it; `outcome: succeeded` requires none, and is **not** a no-op — it opts the job out of the clearing rule above, so a plan failure stays a failure.

  This exists because `assert.execution` structurally cannot express "this job should have failed": a defect that swallows a failure runs the same steps in the same order, so the assert matches either way — and then the match clears the very difference under test. `outcome:` is the observation `execution:` can't make; the two compose.
- **`assert: {stdout, code}`**, on a task/agent step (`code` is task-only — agents have no exit code). A matching `stdout` substring plus exact `code` makes a non-zero-exit task a *success*; a mismatch is a task-level failure, so `on_failure` fires. When present, assert takes over success determination — a task's `fix:` isn't consulted. This folds into the step's content hash, since it changes the success criteria:

```yaml
jobs:
- name: expected-exit
  plan:
  - task: probe
    run: |
      echo checking
      exit 3
    assert:
      stdout: checking
      code: 3
```

- **`assert.verdict`**, on an agent step that declares `verdicts:`: the name the model must have emitted, matched exactly. A classifier's product *is* the choice, so a fixture used to pass on whatever the model decided:

```yaml test=control-classifier
agents:
- name: triage
  source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
- name: label
  plan:
  - agent: triage
    prompt: "Classify this report: the app crashes on launch."
    verdicts: [bug, feature, question]    # all bare: record the choice, route nowhere
    assert:
      verdict: bug
```

  Naming a verdict outside the declared list, or setting it on a step with no `verdicts:`, is a load error: the assert could never match on any run.
- **`assert.tool_calls`**, on an agent step only: an ordered list of `{name, args}` entries the model's tool calls must satisfy, as an ordered subsequence (extra calls are fine) with subset-matched `args`. Values compare as strings. The trajectory records every call the model requested with its own arguments, *before* any `max_calls:` budget check or `args:` pinning — so a budget-rejected call still appears, and a pinned value is deliberately not matchable (asserting on a pinned key is caught at load time). `stdout`, `verdict` and `tool_calls` are ANDed when all are set.
- **`assert.files`**, on a task or agent step: every listed path must exist as a **non-empty** file among the step's captured outputs, checked in the step's own working directory before capture. It's the one assert that checks what a step *wrote* rather than what it *said* — an agent (or a task's `run:`) that reports success in prose without producing anything is otherwise undetectable by `stdout:`/`code:` alone. Each path is artifact-relative: the first segment must name one of the step's `outputs:`, the same rule `context_paths:` follows.

```yaml
jobs:
- name: wrote-something
  plan:
  - task: draft
    run: |
      mkdir -p answer
      echo "the answer is 42" > answer/reply.md
    outputs: [answer]
    assert:
      files: [answer/reply.md]
```

  A missing file, an empty file, or a path naming a directory instead of a file all fail the assert — a load error catches a path that names no declared output before the pipeline ever runs.
- **`steps test <pipeline.yml>`** runs every job in declaration order (forced, so the execution log is deterministic), prints per-job PASS/FAIL, and checks the pipeline-level `assert.execution`. It is how every example in these docs is verified.

## `do:` — several steps as one

A plan is already sequential, so `do:` is not about ordering. It is about **containment**: the block is a single plan step, so one hook on it observes the whole group's outcome.

```yaml
jobs:
- name: ship
  plan:
  - do:
    - task: migrate
      run: echo migrating
    - task: deploy
      run: echo deploying
    - task: smoke-test
      run: echo smoke passed
    on_failure:
      task: rollback           # fires if ANY of the three failed
      run: echo rolling back
```

Without it that rollback has two spellings and both are worse: repeat the hook on all three steps and keep them in sync, or hoist it to the job, where it also fires for failures that have nothing to do with the group.

- **A failing step stops the block**, and the block reports that failure. Deliberately unlike `across:`, which runs every cell: a matrix asks which combinations work, while a `do:` block is one piece of work spelled in several steps — deploying after the migration failed is not a partial answer, it is a worse outcome.
- **The block records nothing of its own.** It is a container, like `in_parallel:`; its children record themselves in declaration order, so `assert.execution` names the children and then the hook.
- **Artifacts flow through in order.** A child may consume an earlier child's output exactly as two consecutive plan steps do. What the block produces stays visible to steps after it.
- **The block takes no operation fields.** `inputs:`, `run:`, `image:`, `prompt:` and friends belong on the steps inside; a block fetches nothing and runs nothing of its own.
- **`try:` works inside**, tolerating only its own step, exactly as it does in a plain plan.
- **A `get:` is not valid inside**, for the reason it is not valid inside `try:` or a concurrent block: a get fans the remainder of the plan out per version, and inside a block that fan-out has nowhere to go.
- **`to:` and `max_visits:` belong on the block, not on its children.** A child has no plan position to be routed to, so those would load cleanly and never fire. They are load errors on a child, naming the fix.
- **Caching**: the block hashes its children's content in declaration order. Sequence *is* its meaning, so two blocks with the same children in a different order are correctly two different nodes, and moving a step into or out of a block changes its identity.

## `in_parallel:` — several steps at once

A plan is otherwise strictly sequential, so independent work waits on itself: three downloads run one at a time, and one slow resource check stalls everything behind it.

```yaml
jobs:
- name: verify
  plan:
  - in_parallel:
      limit: 2          # max in flight; omit for unbounded
      fail_fast: true   # cancel the siblings on the first failure
      steps:
      - task: lint
        run: echo linting
      - task: test
        run: echo testing
      - task: vet
        run: echo vetting
```

- **The block fails when any branch fails.** `fail_fast:` decides only whether the siblings are cancelled or allowed to finish — never whether the failure counts. That distinction is not hypothetical: the first implementation swallowed a child failure under `fail_fast: false`, and a job containing a failing parallel step reported PASS.
- **Every failure is reported**, not just the first. Debugging a parallel block, the useful question is whether one branch broke or all of them did.
- **Classification follows the worst branch.** An errored branch (infrastructure) outranks a failed one (the step said no), so `on_error` fires rather than `on_failure`.
- **Branches start in declaration order** and, with `limit:`, are admitted in that order.
- **`assert.execution` stays deterministic.** Branch names are recorded in declaration order, not completion order. The block itself records nothing — it is a container.
- **A branch cannot consume a sibling's output.** Concurrent branches have no order between them, so that would be a race; the plan-time answer is "that artifact is not available here" rather than "sometimes". Everything the branches produce IS available to the steps after the block. Two branches declaring the same output name is a load error, including across nested blocks.
- **The block itself takes no operation fields**, same as `do:`.
- **`limit:` and `fail_fast:` are hashed**, unlike `attempts:`/`timeout:`/`budget:`. They are not "how hard to try": they change which steps run at all, so a cached result from one setting must not satisfy the other.

To regression-test a parallel block, use `assert.outcome` — `assert.execution` alone structurally cannot catch a swallowed branch failure (both builds run the same branches, so the assert matches either way and then *clears* the difference under test).

## `race:` — first success wins

Run a fast/cheap path and a slow/reliable path at the same time, keep whichever finishes successfully first, cancel the other:

```yaml
jobs:
- name: summarize
  plan:
  - race:
      steps:
      - task: fast
        outputs: [summary]
        run: echo quick take > summary/text.txt
      - task: slow
        outputs: [summary]
        run: sleep 2 && echo thorough take > summary/text.txt
  - task: read
    inputs: [summary]              # the winner's summary/, whoever wrote it
    run: cat summary/text.txt
```

### ⚠️ This costs more, not less

Running both branches **always costs both**, every time, even when the fast one wins. The value is "never wait for the slow path", not "spend less". If you reached for `race:` to save money, you want caching or plain retries instead.

### ⚠️ It is unsafe for side-effecting steps

Cancelling a loser stops only **future** work. A branch that already filed an issue, sent a notification, or pushed a commit keeps that side effect. `race:` is safe for read/generate-only steps; for anything that changes the outside world, run one branch.

Cancellation is also *bounded*, not instantaneous: killing `sh -c "sleep 5; …"` kills the shell but not the `sleep` it forked, so a cancelled step is given a short grace period before it is abandoned.

The rest of the rules:

- **"First success" means completed without error** — not a judgment about output quality. A fast but mediocre answer still wins. Gate quality with a downstream `assert:`/`verdicts:` step.
- **The winner's outputs are the step's outputs**, so a downstream step never has to know which branch won. Every branch must therefore declare the *same* `outputs:` — a mismatch is a load error.
- **A race needs at least two branches.** One runner is a step with extra words.
- **When every branch fails, the block fails** and reports all of them. A hedge is not a guarantee.

## `across:` — one step, once per combination

Run the same step for every combination of some values, instead of writing out a near-identical step per cell and keeping them in sync by hand:

```yaml
jobs:
- name: matrix
  plan:
  - across:
    - var: go_version
      values: ["1.25", "1.26"]
    - var: package
      values: [agent, pipeline]
    task: matrix-test
    run: echo testing {{ .vars.package }} on go {{ .vars.go_version }}
```

`across:` is a **modifier**, not a container: the step it sits on is still a task (or a put, or an agent), it just runs once per cell. `{{ .vars.<name> }}` substitutes into the command, the image, the prompt, the working directory, the step's own name, and each entry of an agent step's `context_paths:` — so a fan-out cell can be *handed* the file it was assigned rather than told to go find it.

### The headline: per-cell caching

Concourse re-runs the **entire** matrix on any change. Here each cell is hashed and cached individually, so changing one value in one axis re-runs only the cells that value appears in.

That works because cells are **siblings**, parented on the step before the block rather than on the block itself — the block's own hash folds in every cell, so parenting cells under it would make one cell's edit change every cell's identity, which is exactly the whole-matrix re-run this exists to avoid.

Cells that are puts or agents are never skipped, for the same reasons those steps never are anywhere else: side effects and non-determinism.

### The rest

- **Cells run in declaration order, not concurrently** — unless the step says `max_in_flight:` (below). Put an `in_parallel:` inside a cell if a cell's own work should overlap.
- **A failing cell does not stop the others.** A matrix asks "which of these combinations work", and stopping at the first red one answers that for exactly one cell. Every failure is reported.
- **Cells are named for their coordinates** — `check [mode=fast suite=unit]` — unless you interpolate a variable into the name yourself.
- **An empty axis, a duplicate `var:`, or a misspelled `{{ .vars.x }}` are load errors.** Each would otherwise mean silently running the wrong matrix.

### A width a step decides: `from_file:`

An axis can take its values from a JSON array an earlier step wrote, so the matrix is as wide as that step said — "review each of these findings", where nobody knew the findings when the pipeline was authored:

```yaml
jobs:
- name: fan-out
  plan:
  - task: scan
    outputs: [findings]
    run: printf '["alpha","beta"]' > findings/items.json
  - across:
    - var: item
      from_file: findings/items.json    # instead of values:
    task: investigate
    inputs: [findings]
    run: echo investigating {{ .vars.item }}
```

This is "the step plans, the pipeline executes": one step produces a work list, and each item becomes its own cell — independently hashed, cached and reported — instead of one agent grinding through the whole list in a conversation that outgrows its window.

**Nothing carries the list but an ordinary artifact.** The producer declares `outputs:` as it would for any other file; the axis names a path inside that artifact. The first path component is the artifact, exactly as it is for `dir:`, and it must be fetched or produced earlier in the plan — a load error otherwise.

- **A JSON array of strings.** Anything else — an object, a list of objects, numbers, unparsable text — fails the block naming the file. An item that needs structure carries a *path*: the producer writes one file per item, the array holds the paths, and cells take `context_paths: ["{{ .vars.item }}"]` (which renders per cell).
- **`values:` and `from_file:` are mutually exclusive per axis**, and an axis needs one of them. Otherwise a file axis is an ordinary axis: it may sit beside static axes or other file axes; the product is taken as usual.
- **An empty array runs nothing, and says so** — `across: 0 cells (findings/items.json is empty); nothing to run` — and the plan carries on. "The scan found nothing" is a legitimate success; a pipeline that wants an empty list to fail asserts that where the file is written.
- **At most 1000 items**, and the file itself is capped at 1 MiB. Nobody reviewed the length of an array a model wrote, and an unbounded one turns an upstream typo into an unbounded bill.
- **The path is confined at load**: absolute paths and any `..` that climbs out of the workspace are load errors.
- **Planning**: a static matrix expands at load, so `steps plan` shows every cell. A file matrix can't — its width isn't knowable until the producing step runs — so it hashes its *declaration* at plan time, and its templates are still checked at load. A chain through any `across:` block is unskippable, which here is load-bearing: **the producing step re-runs every run**, so the file the axis reads is always the file this run wrote. The cells themselves still hash and cache individually.

### Collected outputs: `outputs:` on the matrix

A matrix's cells are clones of one step, so they declare the same output names — which used to mean the last capture silently erased the others'. `outputs:` on an `across:` step means the block **collects**: each cell writes its declared output exactly as any step does, and the capture lands under the cell's own coordinates. Downstream consumes the lot as ONE artifact:

```yaml
jobs:
- name: review-matrix
  plan:
  - across:
    - var: dim
      values: [api, errors]
    outputs: [findings]                  # the block collects
    task: review
    run: echo {{ .vars.dim }} looks fine > findings/report.txt
  - task: merge
    inputs: [findings]                   # findings/api/report.txt, findings/errors/report.txt
    run: cat findings/*/report.txt
```

The cell's own view is unchanged — it writes `findings/report.txt` and never sees the coordinates. The directory layout is one segment per axis value, declaration order, so it is derived entirely from declared things.

- **Collision-proof by construction.** Each cell captures to its own destination, so N cells share one declared name without clobbering each other, serially or concurrently. For a `from_file:` matrix it is the only shape that can work at all, since per-cell artifact names cannot be load-validated when the width is decided mid-run.
- **Axis values become directory names**, so on a collecting matrix they must match the artifact-name charset and be unique within their axis — a load error for `values:`, a read-time error naming the item for `from_file:`.
- **A failed or `try:`-tolerated cell contributes no directory** — capture only runs on success — and the consumer walks what survived. This holds all the way down to nothing surviving: the block creates the artifact **empty** before its cells run, so a matrix whose cells all failed hands the consumer an empty directory, not a missing input.
- **The block replaces the artifact wholesale, each run** — an earlier same-named artifact or a wider expansion's stale directories can never merge into this run's collection.
- **The collect position is the only place a cell may declare outputs.** An output buried in a hook or a nested block would be captured at its plain name by every cell — the exact clobber collection removes — so it is a load error. So are `outputs:` on a `try:` wrapper around the matrix and a collecting matrix wrapping another `across:` step.
- **Requires `strategy: copy` (the default);** `btrfs` is refused for now rather than silently corrupting.
- **Declared on the step, not inherited** from a `tasks:` entry — the step is where a reader looks to see whether a matrix collects.
- **A task cell that produces outputs is never cell-cached.** A skipped cell captures nothing, so a rerun would hand the consumer a directory with a hole where that cell's contribution was. Re-running is the only honest answer.

### A ceiling that degrades: `budget:`

A wide matrix of agent cells is one whose total cost is easy to underestimate — especially when a step decided the width mid-run. `budget:` on the block caps what its cells spend **together**:

```yaml test=control-across-budget
agents:
- name: reviewer
  source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
- name: bounded-review
  plan:
  - across:
    - var: dim
      values: [api-boundaries, error-paths, concurrency]
    budget:
      tokens: 1000            # cells stop being admitted once this is spent
    agent: reviewer
    prompt: "Review the {{ .vars.dim }} dimension."
  - task: publish
    run: echo publishing what we got
```

```
budget: across stopped after 1 of 3 cells (spent 2,000 of 1,000 tokens)
```

- **It stops, it does not fail.** When the allowance is gone the matrix starts no further cell, the cells already running finish and keep what they recorded, and **the plan carries on**. That is the opposite of the job and agent ceilings, and deliberately so: "review eight of the twelve dimensions and publish" beats "spend the same money and publish nothing".
- **Checked before a cell starts, never mid-cell.** A cell that has begun runs to completion.
- **A rerun finishes the work.** The cells that ran are recorded; running again with a larger allowance picks up where the last one stopped.
- **Tokens only.** `usd` is enforced inside a CLI agent's own subprocess, against itself — it cannot see what the matrix's other cells have spent. Rejected at load.
- **Measured as a delta on the job's own accumulator**, so a cell's retries and sub-agents count. Not hashed, like every other operational limit.
- **It is not a hard cap.** A cell that overruns is not cut off mid-flight. What bounds the overshoot is the reservation below, not the ceiling itself.

#### Making it bind at any width: `reserve_per_cell:`

Admission can only see what **finished** cells reported, and a cell only finishes once something waits for its slot. So on its own the ceiling is blind to the cells it is deciding against: the first `max_in_flight:` cells are admitted against a total of ~0, and at a width covering every cell there is no serialization point anywhere in the block, so the budget bounds *nothing*.

A reservation fixes that by charging the allowance up front for work not yet reported:

```yaml fragment
- across:
  - var: dim
    from: dimensions
  max_in_flight: 6            # real concurrency, not throttled to make the budget work
  budget:
    tokens: 3600000
    reserve_per_cell: 600000  # what admission assumes an unfinished cell will cost
  agent: reviewer
```

Six cells at 600K reserved exactly covers the 3.6M allowance, so the sixth is admitted at the line and a seventh is not. Cells that come in **under** their reservation release the difference and let later cells through — the common case, and the reason this is a reservation and not a hard pre-allocation.

- **Where the number comes from**, first match winning: the block's `reserve_per_cell:`, then the cell agent's own `budget.tokens` (you already declared what one invocation may cost, so a block often needs no new config at all), then nothing — and with nothing, the block admits exactly as it did before reservations existed, warning included.
- **A too-large reservation under-admits**, stopping the matrix earlier than the allowance strictly required. That is the safe direction, and a rerun with a larger allowance resumes where it stopped.
- **Across-only.** The same field on an agent's or a job's `budget:` is a load error: neither admits anything, so a reservation there would read like configuration and bind nothing.
- **A CLI agent supplies no reservation.** Its ceiling is `budget.usd`, metered in dollars inside its own subprocess, which cannot be compared against a token allowance — such a block needs an explicit `reserve_per_cell:` to bind.
- **The warning survives for the one case that still cannot bind**: a budget with no reservation source at a width covering every cell.

### Concurrent cells: `max_in_flight:`

Serial cells are the right default for a hand-written matrix. They are the wrong default for a wide one — especially a `from_file:` fan-out, where the cells are N independent agents an earlier step decided on:

```yaml
jobs:
- name: wide
  plan:
  - across:
    - var: dimension
      values: [api-boundaries, error-paths, concurrency, performance]
    max_in_flight: 4        # four cells at a time
    task: review
    run: echo reviewing {{ .vars.dimension }}
```

- **Unset or `1` is the serial walk**, unchanged. This is opt-in, unlike `in_parallel:`'s `limit:` where an absent value means unbounded — each default matches the contract its own block already had. A value at or above the cell count is effectively unbounded.
- **Cells are admitted in declaration order.**
- **`assert.execution` stays deterministic**: each cell records into its own log, merged back in declaration order at the join.
- **There is no `fail_fast:`.** A matrix asks which combinations work; every cell runs and every failure is reported, exactly as in the serial walk.
- **Not hashed**, unlike `in_parallel:`'s `limit:`/`fail_fast:`. Those change which steps run at all; this changes only how many run at once — so widening a matrix whose cells are all cached re-runs nothing.
- **Above 1 requires `strategy: copy`** (the default).

## `approval:` — a human in the plan

Agent pipelines eventually gate something that cannot be undone — publishing, deploying, sending. `approval:` is the place in a plan to stop and ask. It parks the run until someone decides, so it can't be executed by the docs suite:

```yaml noexec
agents:
- name: writer
  source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
- name: publish
  plan:
  - agent: writer
    prompt: "Draft the release announcement into draft/summary.md."
    outputs: [draft]
  - approval:
      message: "Draft is in draft/summary.md — publish?"
      timeout: 24h
    on_abort:
      task: nag
      run: echo nobody decided within 24h    # expiry classifies as aborted
  - task: post
    inputs: [draft]
    run: cat draft/summary.md
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

Stated deliberately rather than left to be discovered. There is no separate identity system. The recorded approver comes from `$STEPS_APPROVER`, `$USER`, or `$LOGNAME`: it is an **audit record**, not an authorization check.

### The rest

- **The audit trail is a row, not a chat message.** Who approved, when, and why a rejection was a rejection must not depend on external chat history.
- **A decision cannot be overwritten.** Deciding an already-decided approval is an error; the record is a record.
- **Nothing about an approval is cacheable.** A cached "they said yes once" must never stand in for asking again.
- **Notification is the missing half, and it is not built in.** Today the step prints the message and the exact command to answer it, and logs `job.approval_pending` — put a `put:` to Slack or email in the step *before* the approval to actually reach someone.
