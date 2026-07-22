# Control Flow

Three distinct, easy-to-conflate mechanisms for shaping how a job's plan executes, plus the self-verification (`assert:`) that makes fixtures out of them. All are opt-in; a pipeline that uses none of this hashes and behaves exactly as if the feature didn't exist. See `examples/flow.yml` — every job there is a self-verifying, modelless `steps test` fixture; run it with `steps test examples/flow.yml`.

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
- **No identity of their own**: a hook step records no merkle node and no `job_run` — the enclosing step/job records the aggregate outcome.
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
- **A guard-skipped step skips only itself; the plan continues.** This is different from a merkle cache hit, which stops the whole remaining chain.
- A skipped step fires no hooks and records no merkle node, `job_run`, or execution-log entry — the same contract as a cached skip. That's what lets `assert.execution` prove a step was skipped.
- The guard runs under the step's own resolved image, in a workspace materialized from the step's declared `inputs:`, but closed without capturing outputs — a guard can never publish artifacts.
- The guard command is folded into the step's content hash, but its *outcome* is a run-time fact the planner can't know, so any chain containing a `when:` step is unskippable — never recorded as a reusable "this whole chain succeeded" hash.
- Invalid on `get` steps (a get fans the remainder of the plan out per version, so a conditional get has no coherent meaning).
- This is how an agent decides routing without typed outputs: an `agent` step writes a verdict into a declared output artifact, and the next step's `when:` tests that file. The model proposes; a deterministic command disposes; both are visible in the YAML.

## Step transitions (`to:`/`max_visits:`/`verdicts:`)

A `task`/`put`/`agent` step can carry `to:` — a map that routes to another step **in the same get-segment** based on this step's outcome, including jumping backward to form a bounded loop (a judge/revise cycle). Task loops are self-verifying via `examples/flow.yml`; verdict routing is illustrated in `examples/agents.yml` (needs a model).

- **Routing keys**: `success`/`failure` for a task/put/verdict-less agent, or a verdict name for a verdict agent (below). An errored or aborted step produces no key and never routes — it propagates, so a loop can't spin during shutdown or mask an outage. A `to.failure` route consumes the failure (the job doesn't also fail).
- **Agent verdict routing (`verdicts:`)** is the N-way judge. An agent step declaring `verdicts: [approve, revise, escalate]` gets a synthesized *required* `verdict` tool whose enum is exactly those verdicts — the model must call it, reusing the required-tool `tool_choice` forcing from [agents.md](agents.md) verbatim. The choice becomes the routing key. Every declared verdict must have a `to:` target; `failure:` is reserved for "never emitted a verdict / errored." There's no generic `success` key in verdict mode, and verdict mode is agent-only.
- **`max_visits:`** caps how many times a step executes (per `runSteps` invocation, so `version: every` gives each version its own budget), and is required at load time for any step whose `to:` routes backward. Exceeding it is a job-level failure, reaching the job's own `on_failure`/`ensure` — distinct from the step's own per-iteration `on_failure`.
- **Get-segment restriction**: a `to:` target must be within the same segment (the run of non-get steps between gets) — a jump can't cross a `get` anyway, since the plan re-enters over a truncated slice per version. `to:`/`verdicts:` are invalid on `get` steps and hook steps. Step names must be unique within a `to:`-using segment.
- **Merkle**: `to:`/`max_visits:`/`verdicts:` fold into the step's content hash; `verdicts:` matters because it changes the synthesized tool set. There's no structural merkle change — the planner walks declaration order ignoring `to:` targets, because any routing step's chain is already unskippable, so its plan-time hash is never used for a skip.

## Handoff context (`handoff:`)

Every agent step is otherwise a fresh, hermetic conversation — a step reached via `to:`/`verdicts:` learns nothing about why it was invoked. `handoff:` opts an agent step into transition context on **routed entry only**: the step's first/unrouted execution is unaffected. Illustrated in `examples/agents.yml`'s `judge` job (needs a model).

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
- **Merkle**: only the *declaration* (`context`/`tool` booleans) folds into the step's content hash, value-gated like `image:`/`when:`/`to:` — a step without `handoff:` hashes byte-identically to before this feature existed. The runtime facts a block/tool report (which step routed here, the note, the visit count) are deliberately **excluded** from identity, the same treatment `attempts:` gets — they can't be known at plan time, and agent steps are already unconditionally unskippable, so this never causes a wrong cache hit.

## Assert (self-verification) + `steps test`

`assert:` lets a pipeline verify its own behavior — the mechanism that turns a hooks/control-flow fixture into a runnable regression test. See `examples/flow.yml`, run via `steps test examples/flow.yml`.

- **`assert.execution`**, on a job (ordered task/agent/hook names that must have run) or the pipeline top level (ordered job names). A job's execution is recorded into an in-memory log as the job runs. **A matching job `assert.execution` clears the plan's failure** — evaluated after hooks — so a fixture of deliberately-failing tasks stays green as long as the recorded order matches. A mismatch fails the job with a want/got diff and is never itself cleared. Execution asserts are never hashed (they're meta-checks, like job hooks).
- **`assert: {stdout, code}`**, on a task/agent step (`code` is task-only — agents have no exit code). A matching `stdout` substring plus exact `code` makes a non-zero-exit task a *success*. A mismatch is a task-level failure, so `on_failure` fires. When present, assert takes over success determination — a task's `fix:` isn't consulted. This folds into the step's content hash, since it changes the success criteria.
- **`assert.tool_calls`**, on an agent step only: an ordered list of `{name, args}` entries the model's tool calls must satisfy, as an ordered subsequence (extra calls are fine) with subset-matched `args`. Values compare as strings. The trajectory records every call the model requested with its own arguments, *before* any `max_calls:` budget check or `args:` pinning — so a budget-rejected call still appears, and a pinned value is deliberately not matchable (asserting on a pinned key is caught at load time). `stdout` and `tool_calls` are ANDed when both are set.
- **`steps test <pipeline.yml>`** runs every job in declaration order (forced, so the execution log is deterministic), prints per-job PASS/FAIL, and checks the pipeline-level `assert.execution`. This is the self-verifying-fixture entry point.
- There's no modelless agent fixture for `assert.tool_calls` — it's covered by unit tests instead, because building an agent's LLM client happens inline with no CLI mock seam. Likewise no `on_error`/`on_abort` fixture yet (would need a docker bad-image task / a per-task `timeout:` directive) — the classification and dispatch machinery already supports both, only the deterministic triggers are missing.
