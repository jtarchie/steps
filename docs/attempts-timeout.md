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

## An agent step's `attempts:` multiplies — it does not add

**This is the single most surprising thing about `attempts:` on an agent step.** There are two retry layers stacked on every agent call, and only one of them is yours:

| Layer | Who owns it | Configurable |
|---|---|---|
| HTTP request retry | the LLM client (`openai-go/v3`, `MaxRetries: 2`) | no — not today |
| whole-conversation retry | `steps`' own `attempts:` | yes |

The client retries on connection errors and on HTTP 408, 409, 429, and every 5xx. So one failing turn costs **three** provider requests, not one, and the two layers multiply:

```
provider requests per failing turn  =  attempts:  ×  3
```

- `attempts: 2` → up to **6** requests
- `attempts: 6` → up to **18** requests

Each of those re-sends the entire conversation so far, which makes a retry late in a long conversation one of the most expensive things this system can do. Against a provider that caps spend by the dollar rather than by request rate, an unnoticed 3× can exhaust the budget in a fraction of the planned time.

**This is now visible.** Every request the client is about to retry logs a line, and each failed attempt reports what it actually spent:

```
WRN agent.request_retry  agent=coder model=deepseek-v4-pro attempt=1 of=3 status=500 elapsed=2.1s
WRN agent.request_retry  agent=coder model=deepseek-v4-pro attempt=2 of=3 status=500 elapsed=5.4s
WRN retry.attempt_failed attempt=1 attempts=2 provider_requests=3 error="... 500 ..."
```

Two `agent.request_retry` lines for three requests: the last failure of a burst is not followed by a retry, and `retry.attempt_failed` reports that one with the total beside it. A successful turn logs nothing.

Read it as: *3 provider requests inside 1 conversation attempt, and there is 1 attempt left.* If you were reasoning about cost from `attempt=1 attempts=2` alone, you were off by 3×.

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
