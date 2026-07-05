# incident-runbook

An incident opens, and the machine **survives its own tooling**. A Honeybadger
webhook fires; `steps serve` maps the payload — the fault's class, message,
environment, and counts — straight into the run's inputs and starts it. Then,
in order:

1. **`probe`** fans `http.get` over every service's status endpoint (real HTTP;
   a missing service is a `404`, which is *data*, not an error),
2. **`fetch_fault`** *enriches* the webhook summary with the one thing the
   webhook leaves out — the **backtrace** (Honeybadger's webhook is
   summary-only; the full fault lives behind the Data API). This is best-effort:
   when the tracker is itself unreachable mid-outage, the fetch is
   **dead-lettered** (`catch: action_error`), and the run diagnoses from the
   webhook summary and live probes alone,
3. a cheap **`responder`** diagnoses from the evidence,
4. an **`verify`** auditor judges the responder's *process* from its transcript
   (`history:` — rung 2), not just its conclusion,
5. a senior model **`take_over`** *resumes the responder's actual conversation*
   (`adopt:` — rung 3) when the diagnosis is weak,
6. a human **`pick`**s which remediations to apply (a multi-select gate),
7. a scribe **`apply`**s each one, and the **`report`** always ships
   (`catch: {"*": report}`).

```
                          ┌─ tracker down ─→ note_tracker ─┐
probe ─→ fetch_fault ─────┤                                ├─→ responder ─→ verify
                          └─ fault fetched ────────────────┘                  │
                                                          sound + confident ──┤
                    ┌───────────────── flawed / low confidence ───────────────┘
                    ↓
                 take_over (adopts responder) ─→ propose ─→ pick ⏸ ─→ apply ─→ report ─→ done
                                                              (multi gate)   (catch:* → report)
```

## Triggered by a webhook

The machine declares its own trigger — the `webhook:` block is the contract, the
same way `input:` and `output:` are:

```ts
webhook: {
  path: "honeybadger",
  map: ({ body, hb_base }) => ({
    incident: `Honeybadger fault #${body.fault.id} in ${body.fault.environment}: ${body.fault.klass} — ${body.fault.message} (seen ${body.fault.notices_count}×, last ${body.fault.last_notice_at})`,
    fault_url: `${hb_base}/v2/projects/${body.fault.project_id}/faults/${body.fault.id}`,
  }),
}
```

`map` is a function of one flat scope — `body` (the parsed JSON payload),
`headers`, `query`, plus any operator-supplied `--hook-input` values by name
(here `hb_base`). It returns run inputs; only declared `input:` keys pass
through, and any missing **required** input rejects the POST with `400`. Like
every other function in a machine, `map` is dry-run at load. `steps serve --hook
workflow.ts` registers `POST /hooks/honeybadger`; the POST **durably queues** a
run and returns `202` — gates are still answered in the web UI or CLI (webhook
*resumption* is a later feature). `serve` takes `--hook` more than once to host
several webhooks at once, each bounded by its own `maxInFlight`/`maxQueued`; see
[docs/webhook.md](../../docs/webhook.md) for the full trigger + queue reference.

**The payload is the primary source, not a notification.** Honeybadger's webhook
is [summary-only](https://docs.honeybadger.io/guides/integrations/webhook/) — it
carries the fault's class, message, environment, and occurrence counts, but not
the backtrace. So `map` lifts that summary directly into `incident` (no API call
needed for it), and composes `fault_url` for the one thing the summary lacks.
That split is why `fetch_fault` failing is survivable: the diagnosis always has
the summary; the API call only adds depth.

## The three context rungs in one machine

This is the example's headline: all three declared-access rungs, side by side,
each earning its place.

- **Rung 1 — `ctx`** (everywhere): typed outputs templated into downstream
  prompts. `propose` reads `(take_over || responder).diagnosis`; the report
  zips `probe.items` with the service list.
- **Rung 2 — `history:`** (`verify`): the auditor must judge *process*, so it
  gets a read-only projection of the responder's transcript — **the failed
  first attempt included**. It is data *about* a conversation, not the
  conversation; `from:` must be a graph-predecessor, checked at load.
- **Rung 3 — `adopt:`** (`take_over`): tier escalation. The senior does not
  start fresh — it receives the responder's actual message array and continues
  it. `lastTurns: 2` trims to the responder's final exchange, which is safe
  *only because* the senior's own prompt re-carries the incident. (Drop the
  trim and the original primer — with the fault id — rides along twice.)

## What it exercises

| Feature | Where |
|---|---|
| **Webhook trigger** | `webhook: {path, map}` + `steps serve --hook`; payload → run inputs |
| **`http.get` builtin** | `probe` (fan-out) and `fetch_fault` (single) — real HTTP |
| **`http.get` `headers:`** | `fetch_fault` and the senior's tool send `Authorization` |
| **`catch: action_error`** | the dead tracker dead-letters to `note_tracker`, run continues |
| **`catch: {"*": …}`** | a mis-drafted `apply` step routes straight to `report` |
| **`retry: "none"`** | `probe`, `fetch_fault`, `apply` — evidence/report must not stall |
| **`history:` (rung 2)** | `verify` audits the responder's transcript |
| **`adopt:` other state (rung 3)** | `take_over` resumes `responder` with `lastTurns: 2` |
| **Multi-select gate** | `pick` — `choices: {multi, event, min}`; downstream reads `pick.selected` |
| **`forEach` `index`/`total`** | `apply` numbers each runbook entry |
| **Explicit `system:`** | `responder` and `verify` |
| **`yaml()` / `include()`** | `responder` renders probes as YAML; `report` includes a header template |
| **`structuredOutput: "native"`** | `responder` (tool-less JSON contract) |
| **Agent proposes, guard disposes** | `verify` emits `sound`; a confidence guard vetoes it |
| **Per-state models** | `responder` / `auditor` / `senior` aliases; the senior is a tier up |
| Tool guards (live only) | the senior's `file.read`: `require: "http.get"`, `maxCalls`, `onReject`, pinned `root`/credential `args` |

The senior's tool loop (bare-args `http.get`, `require:`-ordered `file.read`,
machine-pinned credential) is **live-only**: the mock provider replaces models
but cannot script tool calls, so CI never depends on a tool being invoked.

## `http.get`: what is data, what is an error

| Situation | Result |
|---|---|
| `200`, `404`, `500` — any HTTP response | `{status, body}` — **data** |
| connection refused, DNS failure, timeout | `action_error` — routed by `catch:` |

Two consequences shape the machine:

- The unknown `search` service (no status file) returns `404` and flows as
  evidence — the responder must reckon with it, and the auditor checks that it
  did.
- The dead tracker is a **separate single state**, not part of the probe
  fan-out: a `forEach` item failure aborts the whole state and loses the
  aggregate, so `fetch_fault` stands alone with its own `catch:`. Probes carry
  `retry: "none"` — a refused connection is an answer, not something to retry.

## Run it

```sh
# 0. The fixture "infrastructure": status endpoints + the Honeybadger API.
python3 -m http.server 8787 --directory fixtures/serve &

# 1. Webhook mode (the point of this example): serve + trigger.
../../steps serve --hook workflow.ts \
  --hook-input services=api,worker,cache,search \
  --hook-input status_base=http://127.0.0.1:8787/status \
  --hook-input hb_base=http://127.0.0.1:1 \
  --hook-token sekrit \
  --mock mock_responses.yaml &
curl -s -X POST 'http://127.0.0.1:8484/hooks/honeybadger?token=sekrit' \
  -d @fixtures/webhook.json
# → 202; open http://127.0.0.1:8484, watch the run park at the gate, pick
#   remediations 1+2 in the form, and read out/incident-report.md.

# 2. Fast path: the tracker is reachable (hb_base points at the fixture
#    server), the responder is confident, no escalation. Restart serve with
#    --hook-input hb_base=http://127.0.0.1:8787 and --mock mock_fast_path.yaml.

# 3. CLI mode (no daemon), canonical escalation path — parks at the gate:
../../steps run workflow.ts --mock mock_responses.yaml \
  --input incident="Honeybadger fault #83214792 (production): Redis::TimeoutError — Connection timed out after 5000ms in CheckoutController#create" \
  --input services=api,worker,cache,search \
  --input status_base=http://127.0.0.1:8787/status \
  --input fault_url=http://127.0.0.1:1
# then answer inline (a TTY prints numbered options): 1,2

# 4. Validate without running — dry-runs every function, webhook.map included.
../../steps validate workflow.ts --print

# 5. Live: real models, real Honeybadger.
export OPENROUTER_API_KEY=sk-or-...
#    point Honeybadger's webhook integration at /hooks/honeybadger?token=... and
#    pass the API base + auth header + runbook root as hook inputs:
#      --hook-input hb_base=https://app.honeybadger.io \
#      --hook-input hb_auth="Basic $(printf '%s:' "$HB_TOKEN" | base64)" \
#      --hook-input runbook_dir=fixtures/runbook
#    (Honeybadger's v2 Data API is basic auth with the token as the username;
#     confirm the exact scheme against their docs. hb_auth is the FULL header
#     value — goja has no btoa, so the machine cannot encode a raw token.)
```

## Expected mock trace (what CI asserts)

**`mock_responses.yaml` — escalation** (`engine/acceptance_test.go`
`TestIncidentRunbookEscalationTrace`, and the full serve lifecycle in
`cmd/steps` `TestWebhookTriggersIncidentRunbook`):

```
probe ×4 → fetch_fault(action_error) → note_tracker → responder
  (rate_limited → non-JSON → diagnosed@0.55) → verify(flawed)
  → take_over(adopts responder) → propose → pick ⏸  (answer 1,2)
  → apply ×2 → report → done       (10 transitions)
```

Asserted: `probe.count == 4` with the `search` item at HTTP `404`; the
`fetch_fault → note_tracker` catch and the dead-letter artifact; one each of
`action_error` / `rate_limited` / `schema_violation`; the auditor's prompt
embeds the responder's *failed* attempt; the responder transcript is 4 messages
and the senior's adopted transcript is 4 (untrimmed would be 6), with the fault
id appearing exactly once; the multi gate's 3 options; and the shipped report.

**`mock_fast_path.yaml` — report anyway** (`TestIncidentRunbookFastPathReportAnyway`):

```
probe ×4 → fetch_fault(200) → responder(diagnosed@0.9) → verify(sound)
  → propose → pick ⏸  (answer 1) → apply(schema_violation) →catch:*→ report → done
                                                              (8 transitions)
```

No `take_over`, no `note_tracker`; `verify` fires `on: sound` (the guard
passed); `apply`'s lone non-JSON reply routes `catch:"*"` to the report, which
ships with the "runbook steps were not drafted" fallback. The fault fetch
succeeds *through the auth middleware*, proving the `headers:` arg end to end.

## Footguns

- **`memo` would break the adopt/history source.** `responder` is deliberately
  un-memoized: a memo replay records no conversation, which would starve both
  `verify`'s `history:` and `take_over`'s `adopt:`.
- **No `onItemFailure: "skip"` on `probe`.** The report zips `probe.items` with
  the service list *by index*; a dropped item would misalign every row.
- **`adopt` trims by messages, not turns.** `lastTurns: 2` keeps the last two
  *messages*, which here is exactly the responder's final exchange.
- **Never `JSON.parse` in a machine function.** `fetch_fault.body` is a JSON
  string embedded verbatim into the prompt; the model reads it fine, and the
  load-time dry-run doesn't choke on a stubbed string.

## Not covered here

`maxCost` is inert until a per-model pricing table exists (budgets use
`maxTokens`); `toolChoice: required/one_of` is still unimplemented; the `gh.*`
action pack remains unexercised; and webhook **resumption** (answering a gate
via webhook) is a later feature — this example is trigger-only.
