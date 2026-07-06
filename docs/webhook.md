# webhook — HTTP-triggered runs

**Status:** implemented (2026-07-05) — the trigger contract lives in
`machine.WebhookSpec` (`machine/machine.go`), parsed by `applyWebhook`
(`machine/jsload.go`) and dry-run at load; the daemon side is `steps serve`
(`cmd/steps/serve.go`) plus `handleHook` and the durable-queue `dispatcher`
(`cmd/steps/server.go`, `cmd/steps/dispatcher.go`). Durable queuing rides on a
new `run_enqueued` journal event and a `queued` run status (`journal/`).
Acceptance coverage: `cmd/steps/serve_test.go` (`TestHookMultiRouting`,
`TestHookQueueFull429`, `TestHookDurableQueueDrain`,
`TestWebhookTriggersIncidentRunbook`).

A machine declares its own trigger. The `webhook:` block is the contract, the
same way `input:` and `output:` are — `steps serve --hook workflow.ts` reads it,
registers `POST /hooks/<path>`, and every accepted POST becomes a **durably
queued** run that a dispatcher starts under per-hook and global concurrency
caps. Trigger-only: gates are answered in the web UI or CLI, never the webhook.

## The contract

```ts
webhook: {
  path: "honeybadger",              // URL slug under /hooks/; defaults to the machine name
  map: ({ body, headers, query, hb_base }) => ({
    incident: `Honeybadger fault #${body.fault.id}: ${body.fault.klass}`,
    fault_url: `${hb_base}/v2/projects/${body.fault.project_id}/faults/${body.fault.id}`,
  }),
  maxInFlight: 2,                   // concurrent runs of THIS hook (default 1)
  maxQueued: 50,                    // runs waiting for a slot (default 100) — overflow → 429
}
```

`map` is a function of one flat scope — `body` (the parsed JSON payload),
`headers`, `query`, plus any operator-supplied `--hook-input` values by name. It
returns run inputs; only declared `input:` keys pass through, and any missing
**required** input rejects the POST with `400`. Like every other function in a
machine, `map` is dry-run at load, so a broken mapping fails `validate`, not
production. `path` must be a URL-safe slug; `maxInFlight`/`maxQueued` must be
`>= 0` (`0` means "use the default").

**The payload is the primary source, not a notification.** Lift what the webhook
already carries straight into inputs; reach back to an API (via `http.get`) only
for what the payload omits. See `examples/incident-runbook/` for the full
pattern — a summary-only webhook whose backtrace fetch is best-effort.

## Serving one or many

`--hook` is repeatable; each file must declare a `webhook:` block with a
**unique path and a unique machine name** (the dispatcher keys per-hook limits
by name).

```sh
steps serve \
  --hook incident.ts \
  --hook deploy.ts \
  --hook-input hb_base=https://app.honeybadger.io \
  --hook-token honeybadger=$HB_HOOK_SECRET \
  --hook-token deploy=$DEPLOY_HOOK_SECRET \
  --hook-token fallback-secret \
  --max-in-flight 8
```

- **`--hook-input key=value`** (or `key=@file`) is global: every hook keeps only
  the inputs it declares, so a shared pool usually just works. Two hooks that
  declare the _same_ input name share the value — rename to disambiguate.
- **`--hook-token path=secret`** scopes a secret to one hook; a **bare** value
  (no `=`) is the fallback for any hook without its own. A hook with no token —
  no per-path secret and no fallback — is unauthenticated. The token is sent as
  `Authorization: Bearer <v>` or `?token=<v>`.
- **`--max-in-flight N`** caps concurrently _executing_ runs across all hooks
  (default `NumCPU`). It is the host's safety valve above each hook's own
  `maxInFlight`.

Omit `--hook` entirely and `serve` is just the read-only run dashboard.

## Limits, queue, and backpressure

An accepted POST does not run inline — it writes a `queued` run row (inputs
pinned in a `run_enqueued` event) and returns `202`. A single dispatcher
promotes queued rows to `running` in FIFO order, acquiring a global slot and the
hook's own `maxInFlight` slot before each start. A parked run frees its slot
immediately (the gate is answered later, out of band), so a workflow that waits
on a human never ties up concurrency.

| Situation                                       | Response                                |
| ----------------------------------------------- | --------------------------------------- |
| payload mapped, queue has room                  | `202` `{machine, run, status:"queued"}` |
| payload is not a JSON object                    | `400`                                   |
| `map` threw, or a **required** input is missing | `400`                                   |
| token required and absent/wrong                 | `401`                                   |
| no hook registered at that path                 | `404`                                   |
| the hook already has `maxQueued` runs waiting   | **`429`** — retry later                 |

**Durability is the point of the queue.** Queued rows live in SQLite, so a
`serve` restart re-scans and drains them — nothing accepted is lost. Because the
timeout baseline starts at _dispatch_ (`run_started`), not at _enqueue_
(`run_enqueued`), queue wait never counts against a run's `limits.timeout`.

## Footguns / not covered

- **Unique paths and names.** Two `--hook` files with the same `webhook.path`
  (or the same machine `name`) is a startup error, not a silent last-wins.
- **`maxQueued` is a soft cap.** Concurrent POSTs count-then-insert, so a burst
  can overshoot the limit by a few. It bounds memory and signals backpressure;
  it is not a hard admission gate.
- **Resumes bypass `--max-in-flight`.** Answering a parked gate launches its own
  background resume — the global cap governs webhook _starts_, not resumes.
- **One `serve` per journal.** The dispatcher is per-process; two `serve`
  processes on one DB would both drain the queue and double-dispatch.
- **Webhook resumption is a later feature.** A webhook can _start_ a run, not
  _answer its gate_ — that is still the UI's or CLI's job.
