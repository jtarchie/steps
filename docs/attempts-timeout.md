# Attempts and Timeout

Two operational limits available on all step types (get/task/put/agent):

- **`attempts:`** — retry count for retrying unmodified operations
- **`timeout:`** — wall-clock deadline per attempt

Neither is ever hashed: adding or changing them invalidates no cache.

## When to use attempts

**Agent steps default to `attempts: 3`** (tasks and puts stay at 1). The asymmetry is the domain's: a task that failed will fail again — same command, same tree — so a retry hides a real failure. A model call that failed usually says nothing about the step and everything about the provider's minute, and under one attempt a single 503 destroyed a six-reviewer fan-out whose other five cells were healthy.

```yaml test=attempts-transient
agents:
- name: reviewer
  source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
- name: review
  plan:
  - agent: reviewer
    attempts: 2                    # pins that a retry absorbs the one provider 500 —
    messages:
      - "Review the release."  # any attempts >= 2 would pass; the default is 3
    assert:
      stdout: Release looks good   # the answer AFTER the absorbed 500
  assert:
    execution: [reviewer]
    outcome: succeeded
```

Use `attempts:` for transient faults — flaky network calls, rate limits, temporary unavailability. Do **not** use it for internal bugs, permanent failures (wrong credentials, missing resources), or anything that needs investigation: if a step fails reliably, fix the underlying issue instead of adding retries.

`attempts:` and `timeout:` sit on any step kind:

```yaml
resource_types:
- name: upstream
  config:
    check: |
      printf '[{"ref": "v1"}]'
    in: echo {{ .version.ref | shellquote }} > ref

resources:
- name: release
  type: upstream
  source: {}

jobs:
- name: fetch
  plan:
  - get: release
    attempts: 3        # retries check and in
    timeout: 2m        # per attempt
  - task: verify
    inputs: [release]
    timeout: 5m
    run: cat release/ref
    assert:
      stdout: v1
  assert:
    execution: [release, verify]
    outcome: succeeded
```

## When to use timeout

Use `timeout:` to prevent hung commands — and don't set it aggressively; a legitimate long-running operation should not be cut short:

```yaml fragment
# DON'T: this will fail a 25-minute test
- task: integration-test
  timeout: 10m

# DO: allow the test its reasonable time
- task: integration-test
  timeout: 1h
```

**Timeout is per-attempt**, not per-step total. With `attempts: 3` and `timeout: 30s`, each attempt gets 30 seconds and the total possible time is ~90 seconds plus backoff.

### A whole job can have one too

`timeout:` on a **job** is a wall-clock ceiling on the entire run — the same ceiling `budget:` gives in tokens, in the other unit. It exists because per-step timeouts do not add up to one: a job whose width is decided at run time (an `across:` block over what an earlier step recorded) can run twelve cells that each finish comfortably inside their own deadline and still take all afternoon.

```yaml
jobs:
- name: bounded
  timeout: 45m
  budget:
    tokens: 2000000
  plan:
  - task: work
    run: echo well inside the ceiling
    assert:
      stdout: well inside the ceiling
  assert:
    execution: [work]
    outcome: succeeded
```

- **Checked between steps, never during one.** The step that is running finishes and keeps its work; the deadline decides only whether the *next* one starts. The price is that a job may overrun by one step's duration — **and that bound is only as tight as your steps are**: a step with no `timeout:` of its own is unbounded, so give long steps their own if you want the job's to be a hard ceiling.
- **A fan-out is checked per unit of work, not per block.** An `across:` matrix or `in_parallel:` block stops admitting cells/branches when the deadline passes, and the job still fails. The bound is what is *admitted*, so a block that started everything at once (`max_in_flight:`/`limit:` at or above the unit count) has nothing left to stop.
- **It fails the job**, as a job-level *failure* (the same class as exceeding `max_visits:`), so the job's own `on_failure` and `ensure` fire. That is where a "this took too long" notification belongs.
- **It does not degrade**, unlike [`budget:` on an `across:` block](control-flow.md#a-ceiling-that-degrades-budget). A job-level limit is a backstop against a run that has gone wrong, and stopping loudly is the right answer.

Those claims are pinned by a deliberately-expired deadline: the step that outlives it finishes and *succeeds*, the next step is never admitted, and the job's `on_failure` — not `on_error` — fires, because an expired deadline is a failure at either level (see [Timeout classification](#timeout-classification)):

```yaml
jobs:
- name: bounded-expired
  timeout: 1s
  on_failure:
    task: overdue
    run: echo the job deadline expired
  on_error:
    task: wrong-class
    run: echo this must not fire
  plan:
  - task: slow
    run: sleep 2                 # outlives the job's deadline, and still succeeds
  - task: never
    run: echo must not run
  assert:
    execution: [slow, overdue]   # slow kept its work; never was not admitted
    outcome: failed
```

### An expired timeout is not retried

When an attempt exhausts its own `timeout:`, the step ends there — the remaining attempts are **skipped**. (A deliberate divergence from Concourse, which retries a timed-out attempt like any other failure — see [conformance.md](conformance.md).) The same work against the same budget expires again, so retrying only doubles the wall clock and the bill. This matters most for `agent` steps, where a retried conversation would be rebuilt from scratch and paid for a second time.

`attempts:` buys retries of a transient fault; it cannot buy more time. If a step legitimately needs longer, raise `timeout:`:

```yaml fragment
# DON'T: 2 x 20m of a review that needs 25m, then a failed job
- agent: reviewer
  attempts: 2
  timeout: 20m

# DO: give it the time, keep the attempts for transport faults
- agent: reviewer
  attempts: 2
  timeout: 40m
```

A job-level abort (SIGINT/SIGTERM) is distinct and still stops everything immediately. A deadline from *inside* an attempt — an MCP or HTTP client's own timeout, say — is a normal transient failure and stays retryable; only the step's own `timeout:` ends the retry loop.

## Interaction with `fix:`

A task step can name a **fix agent** — invoked when the task fails, running *inside the failing task's own working directory* (the exact state the task failed in), after which the task re-runs. This example is real end to end: the task fails because a file is missing, the fix agent writes it, the re-run passes:

```yaml test=attempts-fix
agents:
- name: fixer
  source: { model: openrouter/qwen/qwen3.7-flash }
  tools: [read_file, write_file, run_shell]

jobs:
- name: build
  plan:
  - task: check
    run: test -f patched.txt      # fails until the fixer creates it
    fix: fixer
    assert:
      code: 0                     # judged after the repair, on the re-run
  assert:
    execution: [fixer, check]     # the fixer ran, then the task's re-run passed —
    outcome: succeeded            # a green first try would record [check] alone
```

With both `attempts:` and `fix:`, the fix agent runs **once per exhausted attempt**: attempt fails → fix agent → re-run, repeated until an attempt passes or all are spent, and the job fails with the error from the last attempt. Each fix invocation gets its own `attempts:` budget (default 1; overridable with `fix: {attempts: ...}`).

## Interaction with `assert:`

Only the **final attempt's output** is evaluated by `assert:`. If attempt 1 prints the expected text but exits nonzero, the task retries — only the last attempt's stdout and code are checked. A `fix:` sits inside an attempt the same way: the assert decides whether the run needs repairing at all, and then judges the re-run that followed it, as in the fixture above. An assert is the oracle over the outcome a step reached, never a substitute for reaching one — which is why one that is already satisfied costs no repair, and why a run that exits 0 and still misses it gets one. (See [control-flow.md](control-flow.md) for `assert:` itself.)

## Hook firing

Hooks (`on_failure`, `on_error`, `ensure`) fire **once per exhausted step**, not per attempt. If a deploy fails on all 3 attempts, `on_failure` runs once with the error from attempt 3.

## Logging

The CLI logs "attempt N/M" markers for each retry, in both CLI output and structured logs (`job.task.attempt`):

```
task: build
task: build (attempt 2/3)
task: build (attempt 3/3)
```

## What `attempts:` costs on an agent

`attempts:` means the same thing on every step kind: **retry the failing operation**. On a task that operation is a command. On an agent it is one HTTP request to the model — not the conversation.

```
provider requests per failing turn  =  attempts:
```

A retry re-sends the failing request and the conversation carries on from exactly where it was. Nothing accumulated is lost, and a transient 500 costs one extra request rather than a whole re-billed history.

Every retry is logged, and a failed step reports what it really spent:

```
WRN agent.request_retry       agent=coder model=deepseek-v4-pro attempt=1 attempts=3 status=500
WRN agent.request_retry       agent=coder model=deepseek-v4-pro attempt=2 attempts=3 status=500
WRN agent.conversation_failed agent=coder provider_requests=3 error="... 500 ..."
```

Retryable failures are connection errors and HTTP 408, 409, 429, and 5xx. A 400 or a 401 is taken at its word — retrying a request the server rejected on its merits just pays to be told the same thing again.

> ### ⚠️ Breaking change: `attempts:` on an agent used to restart the conversation
>
> It previously threw away every accumulated turn and started over from nothing — closer to *amnesia* than retry, and the restart was incoherent: the agent's **workspace survived but its memory did not**, so a restarted attempt inherited its own half-finished edits with no recollection of having made them. There is also no longer a hidden multiplier (the LLM client used to retry every request twice on its own, so `attempts: 6` meant up to **18** requests); `steps` now owns the retry outright.
>
> **What to change:** nothing, in most pipelines — the new behavior is strictly cheaper and safer. The retry that actually works for a bad *answer* is unchanged: `to:` routing with `max_visits:`, which re-enters the agent with the reviewer's critique in context — a fresh conversation *with feedback*, strictly better than a blind restart.

## Timeout classification

A step's expired `timeout:` classifies as **failed** — `on_failure` fires, `on_error` does not, matching Concourse (which marks a timed-out step failed). The step was given a budget and did not finish inside it: that is the step saying no, not the infrastructure. Errored stays reserved for the machinery breaking (docker, transport, workspace). This fixture pins it — the `assert:` block is what keeps a deliberately-failing job green under `steps test` (see [control-flow.md](control-flow.md#assert-self-verification--steps-test)):

```yaml
jobs:
- name: deadline
  plan:
  - task: slow
    run: sleep 5
    timeout: 1s
    on_failure:
      task: page
      run: echo the deadline expired
    on_error:
      task: wrong
      run: echo this must not fire
  assert:
    execution: [slow, page]    # on_failure fired, on_error did not
    outcome: failed
```

What still lands on `on_error` is the machinery genuinely breaking. A provider that answers nothing but 500s — with `attempts: 1`, so the single request is the whole budget — is that class:

```yaml test=attempts-provider-error
agents:
- name: reviewer
  source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
- name: outage
  plan:
  - agent: reviewer
    messages:
      - "\"Review the PR.\""
    attempts: 1
    on_error:
      task: page
      run: echo the provider is down
    on_failure:
      task: wrong
      run: echo this must not fire
  assert:
    execution: [reviewer, page]   # on_error fired, on_failure did not
    outcome: failed
```

## Agent steps: the one implicit deadline

An `agent:` step's `timeout:` bounds the entire conversation, tool calls included:

```yaml test=attempts-agent-timeout
agents:
- name: reviewer
  source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
- name: review
  plan:
  - agent: reviewer
    messages:
      - "\"Review the PR for safety issues.\""
    timeout: 10m
    assert:
      stdout: No safety issues found
  assert:
    execution: [reviewer]
    outcome: succeeded
```

> **An `agent:` step that sets no `timeout:` still gets one: 30 minutes.** This is the single exception to the explicit-only rule below, and it exists because the OpenAI client sets no request timeout of its own — without a fallback, one hung endpoint blocks the run forever.
>
> **It bounds the step, not each source.** When `fallback:` cascades mid-run (see [agents.md](agents.md)), every source the cascade tries shares this one deadline — `timeout: 10m` across three sources is ten minutes total, not thirty. A source that hangs spends the budget the later ones would have used, which is the point: the ceiling is what the step is allowed to cost.
>
> **Deleting a `timeout:` from an agent step therefore does not remove its deadline — it may shorten it.** Removing `timeout: 45m` leaves 30m, not "no limit". A long-running agent (a coding agent over 100+ turns) needs an explicit generous value. The symptom when 30 minutes isn't enough is `agent: generate content: context deadline exceeded`, classified not-retryable, because a fresh conversation would hit the same wall.
>
> `task:`/`put:`/`get:` steps have no such default: unset means no deadline.

## Put the deadline on the agent, not on every step

`timeout:` and `attempts:` are also fields of an `agents:` entry, and a step that sets neither inherits them:

```yaml test=attempts-agent-dials
agents:
- name: deep-reviewer
  timeout: 20m
  attempts: 5
  max_turns: 50
  source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
- name: review
  plan:
  - agent: deep-reviewer
    messages:
      - "\"Review the diff for correctness.\""
    assert:
      stdout: Looks correct
  - agent: deep-reviewer
    messages:
      - "\"Now review it for security.\""
    timeout: 45m
    assert:
      stdout: No injection paths
  assert:
    execution: [deep-reviewer, deep-reviewer]
    outcome: succeeded
```

Both steps get 20 minutes, five attempts and 50 turns; the second one asks for 45 minutes and gets it. **Precedence is step, then agent entry, then the package default** — the same order `max_turns:` has always had, and nothing about it is new here except that two more dials now participate.

The reason this belongs on the agent is that the right deadline is usually a property of the agent rather than of the step invoking it: a deep reviewer needs twenty minutes whoever calls it. Before an entry could carry one, a deadline shared by six steps was six copies of one number.

It is **not** available on `defaults:`. A pipeline-wide deadline changes failure behavior at a distance — a step written with no `timeout:` means *this one has no deadline*, and a global default silently converts that into a deadline somebody else picked. An `agents:` entry is a thing the author named and pointed at deliberately, which is the difference.

## Zero means no limit

Every dial that bounds an agent step reads the same three ways:

| written | means |
|---|---|
| omitted | take the default |
| `0` | **no limit** |
| negative | load error |

```yaml test=attempts-no-limits
agents:
- name: marathon
  max_turns: 0            # no turn cap — bounded by budget:, timeout: and loop detection
  max_context_bytes: 0    # hand over context_paths: files whole, however large
  timeout: "0"            # no deadline at all
  budget: { tokens: 4000000 }
  source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
- name: migrate
  plan:
  - agent: marathon
    messages:
      - "\"Migrate every call site, however long it takes.\""
    assert:
      stdout: migration complete
  assert:
    execution: [marathon]
    outcome: succeeded
```

Two things about that block are worth being explicit about.

**`timeout: "0"` is quoted, and it only means this on an agent step.** Quoted because YAML would otherwise hand the loader the integer 0 rather than a duration string. Agent-step-only because an agent step is the *only* kind that gets a deadline it never asked for — on a `task:`/`get:`/`put:` step, omitting `timeout:` already means no deadline, so a `0` there says nothing the empty field doesn't and is a load error. Same for a job's `timeout:`.

**Removing a cap is not the same as removing a bound.** `max_turns: 0` does not mean "runs forever": the agent's `budget:` (tokens/USD), the step's deadline, the job's deadline, and loop detection all still bind, and each of them bounds a runaway conversation more precisely than a turn count does. What `max_turns:` is actually for is the case none of those catch — a model that keeps calling tools productively but never converges — so switching it off is a statement that one of the others is the real ceiling for this step, which is why the agent above names a token budget in the same breath.

Take the budget away too, along with `timeout: "0"`, and what is left holding the step is loop detection plus one narrower guard: a model that answers with text through five consecutive **forced** tool calls (see [`required:` tools](agents.md)) ends the attempt, since a provider disregarding `tool_choice` produces no tool interaction for loop detection to hash. That pair is a floor, not a budget — an uncapped agent with no `budget:` and no deadline can still spend a lot before either fires. Removing all three is a choice, not an accident.

**`attempts:` is deliberately outside the convention.** `attempts: 0` is a load error rather than "retry forever", because retrying a provider that is down would never stop, and the backstop that would otherwise catch it — the step's deadline — is itself something a step can now switch off. Omit `attempts:` for the default.

Switching a cap off **busts the cache** for the affected steps. `max_turns:` is part of an agent step's identity (it is hashed, along with the model, persona and tool grant), so changing it re-runs the step — which is correct: a step that may now take 200 turns is not the step that was capped at 30. `timeout:`, `attempts:` and `max_context_bytes:` are never hashed, so changing those re-runs nothing.

## Why no global default?

Timeouts are explicit-only (with the one agent-step exception above): a missing timeout doesn't fail silently, authors are forced to think about reasonable limits per step, and there are no surprise deadlines from a global setting somebody forgot about.
