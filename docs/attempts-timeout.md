# Attempts and Timeout Guide

This guide covers two operational limits available on all step types (get/task/put/agent):
- **`attempts:`** — retry count for retrying unmodified commands
- **`timeout:`** — wall-clock deadline per attempt

## When to Use Attempts

Use `attempts:` to retry transient failures:
- Flaky network calls (e.g., GitHub API rate limits)
- Temporary service unavailability
- Resource contention

Do NOT use `attempts:` for:
- Internal bugs (retrying won't fix them)
- Permanent failures (wrong credentials, missing resources)
- Issues that need investigation (retrying hides the problem)

If a step fails reliably, fix the underlying issue instead of adding retries.

## When to Use Timeout

Use `timeout:` to prevent hung commands:
- Long-running integration tests: `timeout: 30m`
- Network requests: `timeout: 2m`
- Resource fetches: `timeout: 5m`

Do NOT set timeouts too aggressively — a legitimate long-running operation should not be cut short:
```yaml
# DON'T: this will fail a 25-minute test
- task: integration-test
  timeout: 10m

# DO: allow the test its reasonable time
- task: integration-test
  timeout: 1h
```

## Timeout Semantics

**Timeout is per-attempt**, not per-step total. With `attempts: 3` and `timeout: 30s`:
- Each attempt gets 30 seconds
- Total possible time: ~90 seconds (plus backoff between retries)
- Total is not capped at 30 seconds

To implement a total timeout across all attempts, set a longer task-level timeout and a short step-level timeout — but this is rarely needed.

### A whole job can have one too

`timeout:` on a **job** is a wall-clock ceiling on the entire run — the same ceiling `budget:` gives in tokens, in the other unit. It exists because per-step timeouts do not add up to one: a job whose width is decided at run time (an `across:` block over what an earlier step recorded) can run twelve cells that each finish comfortably inside their own deadline and still take all afternoon.

```yaml
jobs:
- name: review
  timeout: 45m
  budget:
    tokens: 2000000
  plan:
  - ...
```

- **Checked between steps, never during one.** The step that is running finishes and keeps its work; the deadline decides only whether the *next* one starts. So a job timeout and a step timeout compose rather than race, and a job never reports a deadline breach against work that was still making progress. The price is that a job may overrun by at most one step's duration — the honest cost of not cutting work off mid-flight.
- **An `across:` block is checked per CELL, not per block.** A whole matrix is one step of the plan, so without this a runtime fan-out — the case this exists for — would never be revisited once it started, and a twelve-cell matrix could overrun by twelve cells. The matrix stops admitting cells when the deadline passes, and the job still fails.
- **It fails the job**, and does so as a job-level *failure* (the same class as exceeding `max_visits:`), so the job's own `on_failure` and `ensure` fire. That is where a "this took too long" notification belongs.
- **It does not degrade**, unlike [`budget:` on an `across:` block](control-flow.md#a-ceiling-that-degrades-budget). Same reasoning as the job's token ceiling: a job-level limit is a backstop against a run that has gone wrong, and stopping loudly is the right answer. Degrading belongs on the block whose width nobody knew when they wrote the pipeline.
- **Never hashed**, like every operational limit — adding one invalidates no cache.

### An expired timeout is not retried

When an attempt exhausts its own `timeout:`, the step ends there — the remaining attempts are **skipped**. The same work against the same budget expires again, so retrying only doubles the wall clock and the bill. This matters most for `agent` steps, where a retry rebuilds the whole conversation from scratch and pays for it a second time.

`attempts:` buys retries of a transient fault; it cannot buy more time. If a step legitimately needs longer, raise `timeout:`:

```yaml
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

When a task step has both `attempts:` and `fix:`, the fix agent runs **once per exhausted attempt**:

```yaml
- task: flaky-test
  run: ./test.sh
  attempts: 3
  fix: fixer-agent
```

If the task fails on all 3 attempts:
1. First attempt fails → fix agent invoked → task re-run
2. Second attempt fails → fix agent invoked → task re-run
3. Third attempt fails → fix agent invoked → task re-run
4. Job fails with the error from attempt 3

Each invocation of the fix agent gets its own `attempts:` budget (default 1; overridable with `fix: {attempts: ...}`).

## Interaction with `assert:`

Only the **final attempt's output** is evaluated by `assert:`:

```yaml
- task: build
  run: ./build.sh
  attempts: 3
  assert:
    stdout: "Build successful"
```

If attempt 1 prints "Build successful" but then fails with exit 1, the assert does not match — the task retries. Only attempt 3's output is checked.

## Hook Firing

Hooks (`on_failure`, `on_error`, `ensure`) fire **once per exhausted step**, not per attempt:

```yaml
- task: deploy
  run: ./deploy.sh
  attempts: 3
  on_failure:
    - task: rollback
```

If the deploy fails on all 3 attempts, `on_failure` runs **once** with the error from attempt 3.

## Logging

The steps CLI logs "attempt N/M" markers for each retry:

```
task: build
task: build (attempt 2/3)
task: build (attempt 3/3)
```

These markers appear in both CLI output and structured logs (`job.task.attempt`).

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

Two retry lines for three requests: the last failure is the one the step reports, with the total beside it. A successful turn logs nothing.

Retryable failures are connection errors and HTTP 408, 409, 429, and 5xx. A 400 or a 401 is taken at its word — retrying a request the server rejected on its merits just pays to be told the same thing again.

> ### ⚠️ Breaking change: `attempts:` on an agent used to restart the conversation
>
> It previously threw away every accumulated turn and started the conversation over from nothing. That was closer to *amnesia* than retry, and it cost three orders of magnitude more than the failure warranted — against a fault the transport layer had already retried and concluded was not transient.
>
> Worse, the restart was incoherent: the agent's **workspace survived but its memory did not**, so a restarted attempt inherited its own half-finished edits with no recollection of having made them. Pipelines needed prompt text (`run git status first, a dirty tree means a previous attempt was cut off`) to work around a config field. Across a real self-build experiment `attempts:` fired five times on agent steps and never once changed the outcome.
>
> There is also no longer a hidden multiplier. The LLM client used to retry every request twice on its own, so the real cost was `attempts: × 3` — `attempts: 6` meant up to **18** requests, each re-sending the entire history. `steps` now owns the retry outright and switches the client's off, so `attempts:` is the whole budget.
>
> **What to change:** nothing, in most pipelines — the new behavior is strictly cheaper and safer, and a bare `attempts: 2` now means "retry a failing request once" instead of "run this conversation twice". If you were relying on a restart to recover from a provider being *down*, that was never what it was good at: see `steps validate`'s preflight checks and agent source failover.
>
> The retry that actually works for a bad *answer* is unchanged and unaffected: `to:` routing with `max_visits:`, which re-enters the agent with the reviewer's critique in context. That is a fresh conversation *with feedback*, which is strictly better than a blind restart.

## Timeout Classification

Timeouts classify as **errored**, not **failed**:
- Failed: task exit code nonzero (recoverable, possibly fixable)
- Errored: timeout, infrastructure error, etc. (usually not fixable, abort immediately)

This distinction affects hook dispatch: `on_error` fires for timeouts, `on_failure` does not.

## Examples

### Get Step with Retries

```yaml
- get: flaky-resource
  attempts: 3
  timeout: 2m
```

Retries version checking (check command) and fetching (in command) up to 3 times, with a 2-minute deadline per attempt.

### Task Step with Timeout and Fix

```yaml
- task: integration-test
  run: ./ci/integration.sh
  attempts: 2
  timeout: 30m
  fix: test-fixer
```

Runs the integration test with a 30-minute deadline per attempt. If it fails, invokes the fix agent once (with its own 1-attempt budget). Then re-runs the test. The final exit code is the step's verdict.

### Put Step with Retries

```yaml
- put: publish-artifact
  attempts: 3
  timeout: 5m
```

Retries the resource's out command up to 3 times, with a 5-minute deadline per attempt.

### Agent Step with Timeout

```yaml
- agent: reviewer
  prompt: "Review the PR for safety issues"
  timeout: 10m
```

Bounds the agent's entire conversation (including all tool calls) to 10 minutes. If the agent hasn't finished by then, the step times out and is classified as errored — and any remaining `attempts:` are skipped, since a restarted conversation would get the same 10 minutes.

> **An `agent:` step that sets no `timeout:` still gets one: 10 minutes** (`agentStepTimeout`, `internal/agent/step.go`). This is the single exception to the explicit-only rule below, and it exists because the OpenAI client sets no request timeout of its own and relies entirely on `ctx` — without a fallback, one hung endpoint blocks the run forever.
>
> **Deleting a `timeout:` from an agent step therefore does not remove its deadline — it may shorten it.** Removing `timeout: 45m` leaves 10m, not "no limit". A long-running agent (a coding agent over 100+ turns) needs an explicit generous value; omit it only if 10 minutes is genuinely enough. The symptom when it isn't is `agent: generate content: context deadline exceeded`, classified not-retryable, because a fresh conversation would hit the same wall.
>
> `task:`/`put:`/`get:` steps have no such default: unset means no deadline (`retryWithTimeout` applies one only when `timeout > 0`).

## Why No Global Default?

The proposal recommends starting with **explicit-only** timeouts (no job-level or pipeline-level default) — with the one documented exception of the agent-step fallback above:
- A missing timeout doesn't fail silently (a timeout without explicit config is a surprise)
- Pipeline authors are forced to think about reasonable limits for each step
- No surprise timeouts from a global setting they forgot about

Future versions could add a `timeout:` field to the `job:` block or `pipeline:` level if needed, without hashing impact (timeout is an operational limit, not content).
