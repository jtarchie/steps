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
    attempts: 2                    # 3 is the agent default; spelled out so this
    prompt: "Review the release."  # fixture pins it — one provider 500 is absorbed
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

### An expired timeout is not retried

When an attempt exhausts its own `timeout:`, the step ends there — the remaining attempts are **skipped**. The same work against the same budget expires again, so retrying only doubles the wall clock and the bill. This matters most for `agent` steps, where a retried conversation would be rebuilt from scratch and paid for a second time.

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
    fix: fixer                    # no step assert here: an assert takes over the
                                  # success decision, so fix: would never be consulted
  assert:
    execution: [fixer, check]     # the fixer ran, then the task's re-run passed —
    outcome: succeeded            # a green first try would record [check] alone
```

With both `attempts:` and `fix:`, the fix agent runs **once per exhausted attempt**: attempt fails → fix agent → re-run, repeated until an attempt passes or all are spent, and the job fails with the error from the last attempt. Each fix invocation gets its own `attempts:` budget (default 1; overridable with `fix: {attempts: ...}`).

## Interaction with `assert:`

Only the **final attempt's output** is evaluated by `assert:`. If attempt 1 prints the expected text but exits nonzero, the task retries — only the last attempt's stdout and code are checked. (See [control-flow.md](control-flow.md) for `assert:` itself.)

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

Timeouts classify as **errored**, not **failed** — `on_error` fires, `on_failure` does not. Failed means the step itself said no (nonzero exit, red verdict); errored means the infrastructure did (timeout, docker, transport). This fixture pins it — the `assert:` block is what keeps a deliberately-erroring job green under `steps test` (see [control-flow.md](control-flow.md#assert-self-verification--steps-test)):

```yaml
jobs:
- name: deadline
  plan:
  - task: slow
    run: sleep 5
    timeout: 1s
    on_error:
      task: page
      run: echo the deadline expired
    on_failure:
      task: wrong
      run: echo this must not fire
  assert:
    execution: [slow, page]    # on_error fired, on_failure did not
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
    prompt: "Review the PR for safety issues."
    timeout: 10m
    assert:
      stdout: No safety issues found
  assert:
    execution: [reviewer]
    outcome: succeeded
```

> **An `agent:` step that sets no `timeout:` still gets one: 30 minutes.** This is the single exception to the explicit-only rule below, and it exists because the OpenAI client sets no request timeout of its own — without a fallback, one hung endpoint blocks the run forever.
>
> **Deleting a `timeout:` from an agent step therefore does not remove its deadline — it may shorten it.** Removing `timeout: 45m` leaves 30m, not "no limit". A long-running agent (a coding agent over 100+ turns) needs an explicit generous value. The symptom when 30 minutes isn't enough is `agent: generate content: context deadline exceeded`, classified not-retryable, because a fresh conversation would hit the same wall.
>
> `task:`/`put:`/`get:` steps have no such default: unset means no deadline.

## Why no global default?

Timeouts are explicit-only (with the one agent-step exception above): a missing timeout doesn't fail silently, authors are forced to think about reasonable limits per step, and there are no surprise deadlines from a global setting somebody forgot about.
