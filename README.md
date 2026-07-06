# steps

A state-machine runtime for LLM micro-agents, in Go. A machine is a
TypeScript file (`workflow.ts`) — transpiled by
[esbuild](https://github.com/evanw/esbuild) and run on
[goja](https://github.com/dop251/goja), both in-process, no Node: plain
state consts plus ONE flow expression — the whole topology visible in one
place, compiled into an enforced graph. Every computed value is a function
of one flat scope, destructured by name. Durable by default. Built on
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
../../steps run workflow.ts \
  --input article=@fixtures/article.txt \
  --mock mock_responses.yaml

# Live, against any OpenAI-compatible local server (LM Studio, Ollama, vLLM):
../../steps run workflow.ts --input article=@fixtures/article.txt
```

A machine can be this small — everything else is defaulted (linear flow,
implicit terminals, retry policies, budgets):

```ts
export default {
  name: "summarize",
  states: {
    draft: "Summarize the article in 3 bullets",
    publish: { write: "out/summary.md", content: ({ draft }) => draft.text },
  },
};
```

When a machine branches, the graph lives in ONE expression — and it is still
a fully enforced state machine (guards select among declared edges; the
engine owns budgets, retries, and loop bounds). The commonest shape — a judge
gating a producer in a bounded revise loop — is one combinator:

```ts
flow: pipe(
  loop(draft, {
    judge: critique,                            // the state whose out-edges the loop owns
    accept: ({ output }) => output.score >= 8,  // exit test on the judge's result
    maxVisits: 3,                               // the ENGINE bounds the loop: visits.critique < 3
    exhausted: branch(escalate, { approved: publish, rejected: fail, timeout: fail }),
  }),
  publish, // accept falls through here
),
```

`loop()` is pure sugar over `branch`: the judge gets exactly
`[accept → then, visits budget → revise, fallback → exhausted]`, and every
existing validation (reachability, terminal proofs, fallback presence) runs
on the result. The judge may be an *action* state — a build command's exit
code (`accept: ({ output }) => output.ok`) judges as well as a model's score.
A gate that never loops is just a `branch`; arbitrary fan-out is
`branch(state, { event: when(guard).to(target), else, catch, timeout })`.

Three more sugars keep the common shapes terse — each lowers to the same
enforced graph (`steps validate --print` shows the expansion):

- **`verdict:`** on the judge declares its acceptance test once, so `loop()`
  needs no `accept:` and the criterion stops being restated across the output
  schema, an `events:` list, and a guard: `critique: { …, verdict: ({ output })
  => output.score >= 8 }`.
- **`gate("escalate", { prompt, approve: publish, timeout: "1h" })`** synthesizes
  the human-escalation state (`gate#escalate`) and its branch tail — `approve`
  routes to the target, `rejected`/`timeout` default to fail — so `exhausted:`
  takes a gate instead of a hand-written human state.
- **`forEach: { …, carry: true }`** pairs each fan-out output with its source
  item (`items[i]` becomes `{item, output, index}`), so a downstream state reads
  `items.map(e => e.item.path)` instead of zipping a parallel list back by index
  — and it stays aligned when `onItemFailure: "skip"` drops one.

And `models:` entries may be **tiers** — `scout: { model: "…", reasoning: "low",
memo: true }` — bundling the per-role knobs so states just say `model: "scout"`
instead of restating them. Compare any `examples/*/workflow.ts` with its
`workflow-dsl.ts` twin: same machine, same mock trace, fewer moving parts. Full
guide with before/after for each: [docs/dsl.md](docs/dsl.md).

Because machines are TypeScript, editors type-check them out of the box: each
`workflow.ts` opens with `/// <reference path=".../docs/src/global.d.ts" />`,
so `pipe`/`branch`/`loop`/`when`/`done`/`fail`/`list` and the `Machine` shape all
autocomplete (add `satisfies Machine` for full structural checking). And
`steps validate` **dry-runs every function** against schema-derived stubs —
a typo like `({ scout_file })` fails at load, naming the available fields,
before any token is spent. `steps context machine.ts` prints what each
state's functions may reference.

## CLI

| Command | What it does |
|---|---|
| `steps validate machine.ts [--print]` | Structure checks + a stub dry-run of every function (unknown-field access fails with the available fields listed). `--print` shows the defaults-expanded machine. |
| `steps run machine.ts --input k=v\|k=@file [--mock file] [-v]` | Start a run. Default output is condensed — one line per state (event, tokens, duration, a result hint); `-v` narrates every message/tool call/transition, `-vv` adds full payloads and thoughts. On a TTY, human gates prompt inline (`--no-prompt` to opt out). JSON summary to stdout. |
| `steps resume <run-id> [--event X --data '{...}']` | Answer a parked human gate, or continue a crashed run from its journal. With no `--event` on a TTY, the gate's choices are presented inline. |
| `steps runs` | List runs and their status. |
| `steps serve [--addr host:port] [--machine wf.ts ...] [--hook wf.ts ...] [--hook-input k=v] [--hook-token t] [--max-in-flight N]` | Web view of runs (default `127.0.0.1:8484`): list with status filters and per-machine token/cost totals, run detail (inputs, a per-execution table, a chronological timeline threading each state's prompts/replies/tools and the transition it took, artifacts, and journaled output/content), and a form to answer parked gates. `--machine` is repeatable: each registered machine gets a `/machines/<name>` page whose form is generated from its `input:` block — one text field plus a file-upload alternative per input (the file's contents become the value, like the CLI's `--input k=@file`), plus a raw-JSON escape hatch for undeclared keys. Submitting **durably queues** a run (`429` when the queue is full) and lands on its run page. `--hook` is repeatable and additive: each machine that declares a `webhook:` block is *also* triggerable at `POST /hooks/<path>`, which maps the JSON payload to run inputs and durably queues a run (`202`; `429` when full), and appears on `/machines` too. Per-machine `maxInFlight`/`maxQueued` bound each; `--max-in-flight` caps all; `--hook-token path=secret` scopes a webhook secret (bare value = fallback). See [docs/webhook.md](docs/webhook.md). |
| `steps context machine.ts [--state s]` | Show what each state's functions may reference, derived from declared schemas. |
| `steps inspect <run-id> [--messages]` | Per-state token usage, failures, and routing from the journal; `--messages` dumps recorded conversations. |

Runs journal to SQLite (`.steps.db` by default, `--db` to move it). Every
prompt, reply, tool call, guard verdict, retry, and transition is an
append-only event — the journal is the audit log, and resume is a fold over
it, never a replay of side effects.

## Human gates

A `human:` state parks the run for a person. The prompt is a string or a
function of scope; routing lives in the flow like any branch. `choices:`
declares how the answer is collected — and every gate also accepts a
free-form `note` merged into its output:

```ts
// Confirm / single choice: each key is one of the gate's resume events.
const escalate: State = {
  human: ({ critique }) => `Score ${critique.score}. Approve or fail?`,
  choices: { approved: "Ship the current draft", rejected: "Fail the run" },
  timeout: "1h",
};
// flow: branch(escalate, { approved: publish, rejected: fail, timeout: fail })

// Multiple choice: options are static or a function of scope; the gate emits
// ONE event and puts the selection in its output as `selected`.
const pick: State = {
  human: "Which modules should be regenerated?",
  choices: { multi: ({ scan }) => scan.modules, event: "chosen", min: 1 },
};
// flow: branch(pick, { chosen: regen })  // downstream: ({ pick }) => pick.selected

// Free-form only (the original shape) stays valid — no choices: needed.
const review: State = { human: "Anything to add before we ship?" };
```

Option events are checked against the gate's branch keys at load; `multi`
functions are dry-run like every other function. The choices are rendered
**at park time** and journaled with the `run_parked` event, so every way of
answering reads the same surface:

- **Inline** — on a TTY, `steps run` prints numbered options and reads a
  selection (a number, an event name, `1,3` or `all` for multi), then a note,
  and continues in-process. `--no-prompt` (or a pipe) keeps park-and-exit.
- **CLI later** — `steps resume <id> --event approved --data '{"note":"…"}'`,
  or `steps resume <id>` with no `--event` on a TTY to pick interactively.
- **Web** — `steps serve` renders the gate as a form (radios / checkboxes /
  free-form + note) that resumes the run server-side.

## Providers

Models are provider-namespaced: `anthropic/claude-haiku-4-5`,
`openai/gpt-4o`, `ollama/qwen3:8b`, `lmstudio/qwen3-0.6b`,
`openrouter/qwen/qwen3-coder`. Anthropic and OpenAI-compatible clients come
from adk-utils-go; `ollama/` and `lmstudio/` are the same OpenAI-compatible
client with different default base URLs (`OLLAMA_BASE_URL`, `LMSTUDIO_BASE_URL`,
`OPENAI_BASE_URL` to override). `openrouter/` (`OPENROUTER_API_KEY`,
`OPENROUTER_BASE_URL`) adds OpenRouter's prompt-caching surface — `x-session-id`
sticky routing keyed on the run id, `cache_control` for Anthropic-routed models,
and cached-token accounting — on a scoped HTTP client. Unlike LM Studio, it
honors `reasoning_effort`, so the `reasoning:` knob actually bounds thinking.
`--mock script.yaml` replaces every model with scripted responses for
deterministic CI.

## Token discipline

The machine, not the transcript, carries the logic — so every context and
output cost is a declared property of a state:

- `maxOutputTokens` (default 2048): no state may generate unboundedly; cap
  exhaustion is a `budget_exceeded` failure, never a hang, never retried.
- `maxInputTokens` (default 8192): the input mirror — no state may *read*
  unboundedly either. The rendered system+prompt is estimated (chars/4)
  before any model call; overflow is a zero-token `budget_exceeded` that
  names the largest inputs (`largest inputs: spec ~6100, plan ~2100`) so the
  fix — `distill:` or trim — is one look away. `maxInputTokens: 0` opts a
  state (or the machine, via `defaults:`) out; implicit distill states are
  exempt, since the distiller is the one place the big payload belongs.
- `maxTurns` defaults by shape: 2 for tool-less states (one model call per
  turn; 2 is headroom), 10 when tools are attached (a tool loop needs room).
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
- `distill: {spec: {for: ({target}) => ..., maxTokens: 400}}` replaces a large
  scope value with a model-extracted slice before the state runs — inside the
  state, `spec` IS the slice. Each entry lowers to a real implicit state
  (`state#spec`) on a cheap `distiller` model: journaled, memoized (unchanged
  source+need pairs replay free), budgeted by `maxTokens`. Never-lose: a
  source that already fits the budget passes through verbatim with no model
  call. forEach consumers distill per item — the slice of the spec that
  matters is a function of the item. See [docs/distill.md](docs/distill.md).
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
machine/    TS loader (esbuild→goja), Dyn values, defaults, schemas, validation, dry-run
journal/    event types, Store interface, SQLite store, fold
engine/     run loop, retries, budgets, handlers (agent via ADK, action, human)
provider/   model-ref registry, mock provider, error classification
toolreg/    named Go functions + builtins (file.write, file.read, http.get, exec.run, diff.split, gh.*)
docs/src/    global.d.ts — ambient TypeScript declarations for machine files
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
- [`examples/codegen/`](examples/codegen/) — spec → working code with **two
  gates**: an LLM reviewer, then a *real* `exec.run` build/test command whose
  exit code routes the flow. The reviewer can be fooled; the build cannot — so
  a build failure loops the coder back with the distilled root cause until it
  goes green. An
  architect plans the files, a coder `foreach`-writes them (raw text, not JSON),
  and the gates run on `openrouter/` where `reasoning:` actually bounds thinking.
- [`examples/incident-runbook/`](examples/incident-runbook/) — a Honeybadger
  **webhook** starts the run (`webhook:` block + `steps serve --hook`). It
  probes services over `http.get`, dead-letters the tracker when it too is down
  (`catch: action_error`), then chains all three context rungs: an auditor
  judges the responder's process via `history:`, a senior model `adopt:`s the
  responder's conversation, and a human answers a **multi-select** gate before
  the report ships. Also the widest single feature spread — `history:`, cross-
  state `adopt:`, `catch: {"*"}`, `retry: "none"`, `structuredOutput: "native"`,
  `system:`, `yaml()`/`include()`.

## Testing

`go test ./...` — no network, no models: the acceptance tests run the examples
against their mock scripts and assert the exact journal traces documented in
the example READMEs (state sequence, retry counts, visit counters, artifacts,
park/resume). The `codegen` acceptance test is scripted for the LLM states but
runs its `exec.run` build gate for real (`bash greet_test.sh`), so it also
proves the ground-truth loop end to end. The `incident-runbook` tests run their
`http.get` probes and fault fetch against a real `httptest` server — actions are
never mocked — and one drives the full `steps serve` webhook lifecycle
(POST → run → park → gate answer → report). Live runs are iteration, not CI.
