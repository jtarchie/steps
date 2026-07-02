# steps

A state-machine runtime for LLM micro-agents, in Go. YAML config over a Go
engine, [Expr](https://github.com/expr-lang/expr) guards, durable-by-default
execution. Built on [google/adk-go](https://github.com/google/adk-go) and
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
../../steps run workflow.yaml \
  --input article=@fixtures/article.txt \
  --mock mock_responses.yaml

# Live, against any OpenAI-compatible local server (LM Studio, Ollama, vLLM):
../../steps run workflow.yaml --input article=@fixtures/article.txt
```

A machine can be this small — everything else is defaulted (linear flow,
implicit terminals, retry policies, budgets):

```yaml
name: summarize
states:
  draft:
    agent: "Summarize in 3 bullets: {{ .ctx.article }}"
  publish:
    action: file.write
    input: {path: out/summary.md, content: "{{ .ctx.draft.text }}"}
```

## CLI

| Command | What it does |
|---|---|
| `steps validate wf.yaml [--print]` | Load-time checks: reachability, guard compilation, event declarations, fallback transitions. `--print` shows the defaults-expanded machine. |
| `steps run wf.yaml --input k=v\|k=@file [--mock file] [-v]` | Start a run; narrates every state, chat message, tool call, retry, and transition to stderr; JSON summary to stdout. |
| `steps resume <run-id> --event X [--data '{...}']` | Answer a parked human gate, or continue a crashed run from its journal. |
| `steps runs` | List runs and their status. |

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
machine/    types, YAML loader (order-preserving), defaults, Expr guards, validation
journal/    event types, Store interface, SQLite store, fold
engine/     run loop, retries, budgets, handlers (agent via ADK, action, human)
provider/   model-ref registry, mock provider, error classification
toolreg/    named Go functions + builtins (file.write, file.read, http.get)
cmd/steps/  CLI + human-readable narration
examples/   canonical examples — they double as the acceptance spec
```

## Testing

`go test ./...` — no network, no models: the acceptance tests run both
examples against their mock scripts and assert the exact journal traces
documented in the example READMEs (state sequence, retry counts, visit
counters, artifacts, park/resume). Live runs are iteration, not CI.
