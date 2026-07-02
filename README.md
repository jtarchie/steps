# steps

A state-machine runtime for LLM micro-agents, in Go. Machines are JavaScript
files evaluated by [goja](https://github.com/dop251/goja) — structure is data,
logic is plain functions, execution is durable by default. Built on
[google/adk-go](https://github.com/google/adk-go) and
[achetronic/adk-utils-go](https://github.com/achetronic/adk-utils-go).

**Thesis:** agents need structure, not vibes. Each state is a hyper-specific
micro-agent with its own model, tools, budget, and typed contract; transitions
are the only way to move; guards are the only way to transition. Determinism
lives at the boundaries, choice lives in the interior. See
[DESIGN.md](DESIGN.md) for the full design.

## Quick start

```sh
go build -o steps ./cmd/steps

# Deterministic demo — no models, no keys, scripted provider:
cd examples/summarize-critic
../../steps run workflow.js \
  --input article=@fixtures/article.txt \
  --mock mock_responses.yaml

# Live, against any OpenAI-compatible local server (LM Studio, Ollama, vLLM):
../../steps run workflow.js --input article=@fixtures/article.txt
```

A machine can be this small — everything else is defaulted (linear flow,
implicit terminals, retry policies, budgets). Any computed value is a plain
function of one scope argument; there are no template or expression
mini-languages:

```js
module.exports = {
  name: "summarize",
  states: {
    draft: { agent: ({ ctx }) => `Summarize in 3 bullets: ${ctx.article}` },
    publish: {
      action: "file.write",
      input: { path: "out/summary.md", content: ({ ctx }) => ctx.draft.text },
    },
  },
};
```

Editors autocomplete the whole DSL via [types/steps.d.ts](types/steps.d.ts)
(`// @ts-check` + a jsconfig), and `steps validate` **dry-runs every function**
against schema-derived stubs — a typo like `ctx.scout_file` fails at load,
naming the available fields, before any token is spent. `steps context
machine.js` prints what each state's functions may reference.

## CLI

| Command | What it does |
|---|---|
| `steps validate machine.js [--print]` | Structure checks + a stub dry-run of every function (unknown-field access fails with the available fields listed). `--print` shows the defaults-expanded machine. |
| `steps run machine.js --input k=v\|k=@file [--mock file] [-v]` | Start a run; narrates every state, chat message, tool call, retry, and transition to stderr; JSON summary to stdout. |
| `steps resume <run-id> --event X [--data '{...}']` | Answer a parked human gate, or continue a crashed run from its journal. |
| `steps runs` | List runs and their status. |
| `steps context machine.js [--state s]` | Show what each state's functions may reference, derived from declared schemas. |
| `steps inspect <run-id> [--messages]` | Per-state token usage, failures, and routing from the journal; `--messages` dumps recorded conversations. |

Runs journal to SQLite (`.steps.db` by default, `--db` to move it). Every
prompt, reply, tool call, guard verdict, retry, and transition is an
append-only event — the journal is the audit log, and resume is a fold over
it, never a replay of side effects.

## Providers

Models are provider-namespaced: `anthropic/claude-haiku-4-5`,
`openai/gpt-4o`, `ollama/qwen3:8b`, `lmstudio/qwen3-0.6b`. Anthropic and
OpenAI-compatible clients come from adk-utils-go; `ollama/` and `lmstudio/`
are the same OpenAI-compatible client with different default base URLs
(`OLLAMA_BASE_URL`, `LMSTUDIO_BASE_URL`, `OPENAI_BASE_URL` to override).
`--mock script.yaml` replaces every model with scripted responses for
deterministic CI.

## Token discipline

The machine, not the transcript, carries the logic — so every context and
output cost is a declared property of a state:

- `maxOutputTokens` (default 2048): no state may generate unboundedly; cap
  exhaustion is a `budget_exceeded` failure, never a hang, never retried.
- `reasoning: low|medium|high`: per-micro-agent thinking budgets (provider
  reasoning effort). A drafting state rarely deserves deep thought; a judge might.
- `structuredOutput: "native"` (opt-in): decoder-constrained JSON on
  OpenAI-compatible backends — zero preamble tokens, no malformed JSON. The
  prompt contract stays as the portable fallback.
- Output schemas double as output budgets: `issues: {type: "array", maxItems: 3}`.
- Reasoning-channel text is journaled (flagged `thought`) but **never** replayed
  on `adopt` and excluded from `history` — scratch thinking is not context.
  Measured on a live 3-revision loop: adopted-prompt growth dropped from
  540→1739→3624 tokens to 495→745→1052.
- `adopt: {from: "self", lastTurns: N}` trims long revision transcripts.
- `memo: true` caches agent outputs by rendered-input hash across runs —
  re-running a review only re-pays for files that changed.
- `model` as a function routes each execution to the cheapest capable model
  (`({lead}) => lead.risk === "high" ? "senior" : "scout"`); `models:` aliases
  keep machines readable and swappable.
- `forEach: {concurrency: N, onItemFailure: "skip"}` fans out in parallel and
  survives poisoned items.

## Failure taxonomy

Three failure classes, three behaviors (per state, overridable):

1. **Transient** (`rate_limited`, `provider_error`, `action_error`,
   `timeout`) — replay the handler, exponential backoff + jitter (3×).
2. **Semantic** (`schema_violation`, `guard_rejected`) — the model broke the
   contract; re-prompt *with the validation error in-conversation* (2×).
3. **Exhaustion** (`budget_exceeded`, `max_transitions`, `run_timeout`) —
   never retried; routed by `catch:` or to the `failed` terminal.

## Package layout

```
machine/    JS loader (goja), Dyn values, defaults, schemas, validation, dry-run
journal/    event types, Store interface, SQLite store, fold
engine/     run loop, retries, budgets, handlers (agent via ADK, action, human)
provider/   model-ref registry, mock provider, error classification
toolreg/    named Go functions + builtins (file.write, file.read, http.get)
types/      steps.d.ts — editor autocomplete for machine files
cmd/steps/  CLI + human-readable narration
examples/   canonical examples — they double as the acceptance spec
```

## Examples

- [`examples/summarize-critic/`](examples/summarize-critic/) — writer/critic
  revision loop: guards, bounded loops, semantic retries, human gate.
- [`examples/summarize-critic-adopt/`](examples/summarize-critic-adopt/) —
  same machine with `adopt: self` conversation continuation; A/B the two
  context philosophies.
- [`examples/pr-review/`](examples/pr-review/) — cheap scouts, expensive
  specialist: `foreach` fans a small model over each file of a diff
  (hermetic context per file), a whole-PR scout adds cross-file leads, and
  the large model only ever verifies flagged files. Trivial PRs never reach
  it. File context flows both ways: `diff.split` deterministically attaches
  the current file to each scout item, and the senior carries a guarded
  `file.read` tool (only PR files, bounded calls, machine-pinned root).

## Testing

`go test ./...` — no network, no models: the acceptance tests run both
examples against their mock scripts and assert the exact journal traces
documented in the example READMEs (state sequence, retry counts, visit
counters, artifacts, park/resume). Live runs are iteration, not CI.
