# steps — a state-machine runtime for micro-agents

**Status:** v1 implemented, 2026-07-02 — see [README.md](README.md); examples pass as the acceptance suite
**Language:** Go · **Config:** YAML over a Go DSL · **Guards:** [expr-lang/expr](https://github.com/expr-lang/expr)
**Stack:** [google/adk-go](https://github.com/google/adk-go) v1 (agent loop, sessions, tools) + [adk-utils-go](https://github.com/achetronic/adk-utils-go) (Anthropic + OpenAI-compatible clients)

> Implementation deltas from this document: the Go builder DSL (`steps.State(...)`)
> is not yet exposed — Go users construct machines via `machine.Parse`; `tool_choice:
> required/one_of` validates as "not implemented in v1"; `history` renders
> messages/tool_calls/thoughts with `last_turns` but no per-turn pairing; costs are
> tracked but no per-model pricing table exists yet (token budgets work).
>
> Token-discipline additions beyond this document (all measured on live runs):
> `agent.max_output_tokens` (default 2048 — no state may generate unboundedly;
> cap exhaustion classifies as `budget_exceeded`, never retried); `agent.reasoning:
> low|medium|high` (maps to provider reasoning effort; per-micro-agent thinking
> budgets); `agent.structured_output: prompt|native` (native = decoder-constrained
> JSON on OpenAI-compatible backends; opt-in because grammar sampling degenerates on
> some local model/backend combos); `adopt: {from, last_turns}` trim; reasoning-
> channel messages are journaled flagged `thought` but NEVER replayed on adopt and
> excluded from `history` unless `include: [thoughts]` — scratch thinking is not
> context. `steps inspect <run-id>` renders per-state token usage and routing.

## Thesis

LLM agents need structure, not vibes. A finite state machine gives you:

- **Legibility** — the entire possible behavior of the system is enumerable at load time.
- **Guardrails** — transitions are the only way to move; guards are the only way to transition.
- **Micro-agents** — each state is a hyper-specific task with its own model, tools, budget, and
  contract, the way a microservice owns one job.
- **Operational semantics** — retries, timeouts, budgets, and exits are properties of the
  machine, not conventions inside prompts.

## Decisions (locked)

| Question | Decision |
|---|---|
| Transition driver | **Agent proposes, guards dispose** — agent output includes a self-declared event; transitions match on `on:` (event) AND `when:` (Expr guard); ordered, first match wins; mandatory fallback |
| State body | **Pluggable handlers** — `agent` (LLM+tools loop), `llm` (single call), `action` (registered Go func), `human` (gate) |
| Data flow | **Explicit contracts** — templated `input:` pulled from run context, typed `output:` schema merged back as `ctx.<state>.*`; fresh conversation per state; declared escape hatches only (`history` projection, `adopt` continuation) |
| Durability | **Durable from day one** — event-sourced journal; runs survive crashes and park on human gates |
| Providers | **Multi-provider from day one** — thin owned interface; Anthropic + OpenAI-compatible adapters |
| Tools | **Registered Go functions** (reflection-derived schemas) + **built-in tool library** (http, json, exec…) |
| Packaging | **Library + CLI** — `steps run`, `steps resume`, `steps runs`, `steps validate` |
| Graph model | **Flat FSM + `foreach` fan-out** — states may map their handler over a ctx list (sequential v1; hermetic per-item contexts); sub-machines and parallel regions remain v1.x |
| Defaults | **Convention over configuration** — linear flow by document order, implicit terminals, default retry/limits; every key optional except `states` and each state's handler |
| Testing | **Mock provider in CI** (scripted responses, fully deterministic), live iteration via Ollama, durability drills; `examples/` double as the acceptance spec |

## Core nouns

- **Machine** — the workflow definition (states + transitions + limits). Immutable, validated at load.
- **State** — one handler + an input contract + an output contract + transitions + retry/catch policy.
- **Transition** — `{on?, when?, to}`. `on` matches the agent-declared event; `when` is a compiled
  Expr guard. Both optional; both must hold if present. Evaluated in order; a bare `{to}` is the fallback.
- **Run** — one durable execution instance: journal + context fold + current position.
- **Handler** — the pluggable state body (`agent`, `llm`, `action`, `human`).
- **Guard** — an Expr expression compiled at load time against a typed environment.
- **Journal** — append-only event log; the run's source of truth.

## YAML sketch

```yaml
version: 1
name: support-triage

input:                          # required run inputs, validated at start
  ticket: {type: object, required: true}

limits:                         # run-level guardrails (all have engine defaults)
  max_transitions: 25           # cycle guard
  max_cost: 2.50                # USD across all providers
  timeout: 30m

initial: fetch_history

states:
  fetch_history:
    action: crm.fetch_history           # registered Go func
    input:
      customer_id: "{{ .ctx.ticket.customer_id }}"
    transitions:
      - to: triage
    catch:
      - match: [action_error]
        to: dead_letter

  triage:
    agent:
      model: anthropic/claude-haiku-4-5
      system: "You classify inbound support tickets."
      tools: [kb.search]                # from the Go registry or builtin lib
      max_turns: 6                      # agent-loop budget
    input:                              # ONLY this crosses the boundary
      ticket: "{{ .ctx.ticket.body }}"
      history: "{{ .ctx.fetch_history.summary }}"
    output:
      schema:
        severity: {enum: [low, high, critical]}
        confidence: {type: number}
        reply_draft: {type: string}
      events: [resolved, escalate]      # injected into the output schema as an enum
    retry:
      - match: [rate_limited, provider_error]      # transient: replay same input
        max_attempts: 5
        backoff: {initial: 1s, factor: 2.0, jitter: true}
      - match: [schema_violation, guard_rejected]  # semantic: re-prompt with the error
        max_attempts: 2
    transitions:
      - on: resolved
        when: output.confidence >= 0.8
        to: send_reply
      - on: escalate
        when: output.severity == "critical"
        to: page_oncall
      - to: human_review                # fallback — agent unsure or guards vetoed

  human_review:
    human:
      prompt: "Agent unsure (confidence {{ .ctx.triage.confidence }}). Approve draft?"
      timeout: 24h
      on_timeout: page_oncall
    transitions:
      - on: approved
        to: send_reply
      - on: rejected
        to: triage                      # loop back; max_transitions bounds it

  send_reply:
    action: mail.send
    input:
      to: "{{ .ctx.ticket.customer_email }}"
      body: "{{ .ctx.triage.reply_draft }}"
    transitions:
      - to: done

  page_oncall:
    action: pagerduty.page
    input: {summary: "{{ .ctx.triage.reply_draft }}"}
    transitions:
      - to: done

  done:        {terminal: true}
  dead_letter: {terminal: true, status: failed}
```

## Defaults — convention over configuration

Every key except `states` and each state's handler is optional. Defaults are applied
*before* validation, so the machine that runs is always fully explicit —
`steps validate --print` shows the expanded form.

**Flow**
- `initial` → the first state declared in the file (document order is preserved).
- A state with no `transitions:` → one transition to the next state in document order;
  the last state → `done`. Linear pipelines need zero transition blocks.
- `done` and `failed` terminals always exist implicitly; declare them only to customize.
- No `catch:` → unhandled exhaustion routes to `failed` with the reason journaled.

**Agent states**
- `model`, `temperature` (0), `max_turns` (10), `max_output_tokens` (2048),
  `structured_output` (prompt), `reasoning` (provider default) cascade:
  state → machine `defaults.agent` → engine option → validation error only if
  still unresolved. `max_turns` bounds model calls per conversation turn and
  resets across semantic retries.
- `agent: "one-line prompt"` scalar shorthand for `agent: {prompt: "..."}`.
- `output` omitted → `{text: string}` and no events (fallback-only routing).
- **The prompt template doubles as the input contract**: templates are Go
  `text/template`, and the validator statically extracts every `.ctx.*` reference —
  so an explicit `input:` block on agent states is only needed to reshape data. If a
  state declares `input:` and no `prompt`, the rendered inputs become the user message.

**Policies**
- Default retry (override per state or in `defaults.retry`; `retry: none` disables):
  transient 3× exponential (1s ×2, jitter, 30s cap); semantic 2× with feedback.
- Default limits: `max_transitions: 50`, `timeout: 15m`; token/cost caps off by default.

The payoff — this is a complete, valid machine:

```yaml
name: summarize
states:
  draft:
    agent: "Summarize in 3 bullets: {{ .ctx.article }}"
  publish:
    action: file.write
    input: {path: out/summary.md, content: "{{ .ctx.draft.text }}"}
```

## Actions vs. agent tools: who chooses?

`tools:` is plural on agents and `action:` is singular on actions — deliberately. A tool
is a runtime-choice interface for a model: the agent decides whether/which to call and
authors the arguments. An action made those choices at authoring time: the rendered
`input:` block *is* the arguments. Plurality only means something when there is a
chooser, and the only chooser in the system is a model. A state that picks between
tools at runtime is an agent and must say so.

The invariant: **one state = one handler invocation = one journal entry = one retry
policy.** Consequences:

- Fixed sequences (fetch → parse → write) are chains of action states — linear-flow
  defaults make this free, and each step becomes independently retryable/resumable
  (parse fails ⇒ resume at parse; the fetch is never replayed).
- Semantically atomic multi-step work is one registered composite Go function.
- Conditional invocation is routing — guards and transitions, visible in the graph.
- Parallel invocation is fan-out (v1.x); plumbing noise in the graph is sub-machines (v1.x).

Possible future sugar if plumbing chains annoy YAML users: `action: [a, b, c]` that
compiles to a chain of *real* states, keeping journal semantics honest. Deferred until
someone actually complains.

## Inside the loop: multiple tools, guarded

The design thesis, stated once: **determinism lives at the boundaries; choice lives in
the interior.** A state's interior may be stochastic because every way it touches the
world passes a deterministic checkpoint. Transitions are the checkpoint at the state
boundary — and **a tool call is the interior's transition**: the moment the model's
choice becomes an effect. It gets the same treatment. Agent proposes, guards dispose,
recursively:

```yaml
triage:
  agent:
    model: anthropic/claude-haiku-4-5
    max_turns: 6
    tools:
      - kb.search                        # bare: unrestricted
      - name: crm.refund
        max_calls: 1                     # per-tool budget within this state
        when: args.amount <= 500 && run.cost < 1.00
        on_reject: feedback              # feedback (default) | fail
      - name: mail.send
        require: kb.search               # only callable after a kb.search call
```

- **Tool guards** are Expr, evaluated at call time. The environment adds `args` (the
  model-authored arguments — the thing being judged), `calls.<tool>` (invocation counts
  this state), and `turn`, alongside the usual `ctx` / `run.*`.
- **`on_reject: feedback`** (default): the guard failure is returned to the model as
  the tool result, so it adapts within the loop — the same retry-with-feedback shape as
  schema violations, bounded by `max_turns`. **`on_reject: fail`** surfaces a semantic
  failure to the state's retry/catch instead.
- **`max_calls`** and **`require`** give the loop budget and ordering constraints
  without promoting every tool call to a full state.
- Every proposed call, guard verdict, and rejection feedback is journaled — the
  interior is stochastic but fully audited.

### The dispatch step

`tool_choice: one_of` — the state completes after exactly one tool call:

```yaml
route:
  agent:
    tool_choice: one_of        # auto (default) | required | one_of
    tools: [billing.lookup, shipping.trace, kb.search]
```

Choosing a tool *is* proposing an event, so in `one_of` mode the chosen tool's name
becomes the state's event and ordinary transitions guard it
(`on: billing.lookup, when: ..., to: ...`). This is the LLM as a typed router:
nondeterministic in *which* branch, deterministic in *shape* — exactly one call,
schema-validated args, guarded routing. It fills the middle ground between `action`
(no chooser) and a free agent loop.

## Fan-out: foreach

A state may map its handler over a list evaluated from ctx:

```yaml
scout_files:
  foreach:
    over: ctx.split_diff.files          # Expr over ctx returning a list
    as: file                            # template variable ({{ .file.path }})
  agent:
    prompt: "What deserves a senior's attention in {{ .file.path }}? ..."
  output:
    schema: {path: string, risk: {enum: [low, medium, high]}}   # PER-ITEM shape
```

- Each item is **hermetic**: agents get a fresh conversation per item — N
  small context windows instead of one big one. This is the context thesis
  applied to scale: the machine assembles per-item context; no item sees its
  siblings.
- The state's ctx entry becomes `{items: [...], count: n}`; downstream guards
  and `over:` expressions compose with Expr builtins:
  `filter(ctx.scout_files.items, {.risk != "low"})`.
- Items share the state's retry policy; templates also see `.index`/`.total`.
- foreach states cannot declare events (no single event exists — route with
  guards over the aggregate), cannot adopt/history, and cannot wrap human gates.
- Sequential in v1 (mock scripts stay deterministic); bounded concurrency is
  the planned extension.

## Context between states: hermetic by default

States are hermetic: a fresh conversation every time, and nothing crosses the boundary
that isn't declared. The invariant is **no ambient context, ever** — "what does this
state see?" must be answerable by reading that state's YAML block alone. When a state
does need access to what came before, there are three rungs of declared access, in
increasing fidelity. Reaching past rung 1 should be a deliberate act.

**Rung 1 — `ctx`: what a state *concluded*.** Typed outputs templated into downstream
prompts (the default, already covered above). If downstream needs "what happened,"
prefer making upstream declare it as an output field (`notes`, `dead_ends`, `sources`)
— the contract stays the interface.

**Rung 2 — `history`: what a state *did*.** A read-only projection of the journal,
rendered as text into a fresh conversation. For verifiers that must judge process, not
just results; for escalation context ("here's what was tried"); for human-gate display.

```yaml
verify:
  agent:
    history:
      from: research
      include: [tool_calls]     # messages | tool_calls (default both)
      last_turns: 10            # default all
      as: trace                 # exposed to templates as {{ .trace }}
    prompt: |
      Did the researcher actually consult real sources?
      {{ .trace }}
```

Hermetic in mechanism: the record crosses as declared input text, statically validated
(`from:` must be a graph-predecessor), journaled like any other input. History is data
*about* a conversation, not the conversation.

**Rung 3 — `adopt`: *becoming* a state's continuation.** The state receives the prior
state's actual normalized message array with its own prompt appended.

```yaml
take_over:
  agent:
    model: anthropic/claude-opus-4-8   # tier escalation: opus resumes where haiku stalled
    adopt: triage
    prompt: "The previous agent could not resolve this. Take over."
```

Earned by two cases: **tier escalation** (tool results already in the transcript are
not re-executed — they may be non-idempotent and they cost money) and **revision
loops** via `adopt: self` (on revisit, continue your own prior conversation instead of
being re-primed). This rung genuinely breaks hermeticity — explicitly, one declared
edge at a time.

Mechanics and rules:

- The journal already stores each state execution's normalized messages (audit,
  resume), so `history` is a render of the journal and `adopt` is a replay of it — no
  new storage, and both survive crash/resume.
- `history.from` / `adopt` targets must be graph-predecessors (load-time check).
  Adopting a state that never executed on this run's path is a semantic failure routed
  to `catch:` — never a silent fresh start. `history` of a never-executed state renders
  empty with a journaled warning. Exception: `adopt: self` on a state's *first* visit
  starts fresh by definition — that is its documented semantics, not an error.
- State budgets still apply: adopted transcripts count toward token/cost limits;
  `max_turns` counts new turns only.
- Cross-provider `adopt` relies on the engine's normalized message format being
  rendered back per-provider; tool-call fidelity across providers is a conformance-
  suite item. Same-provider tier escalation is the primary, trivial case.

## Execution semantics

### The run loop

```
enter state → render input templates → run handler (with retry policy)
  → validate output against schema
  → evaluate transitions in order (event match AND guard) → first match wins
  → journal everything → next state (or park, or terminate)
```

### Failure taxonomy

Three distinct failure classes, three distinct behaviors (shape stolen from Amazon States
Language's `Retry`/`Catch`, which is battle-tested):

1. **Transient** (`rate_limited`, `provider_error`, `action_error`, `timeout`) —
   replay the same handler with the same input, exponential backoff + jitter.
2. **Semantic** (`schema_violation`, `guard_rejected`) — the LLM produced output that failed
   the contract. Retry *with feedback*: the validation error is appended to the conversation
   so the model can correct itself. Bounded attempts.
3. **Exhaustion** (`retries_exhausted`, `budget_exceeded`, `max_transitions`, `run_timeout`) —
   not retryable. Routed by `catch:` blocks to a designated state (e.g. `dead_letter`),
   or the run fails terminally. `catch` supports `match: ["*"]`.

`guard_rejected` is optional per state: by default a failed guard just falls through to the next
transition; a state can opt into treating "agent said `resolved` but the guard vetoed it" as a
semantic retry (tell the agent why).

### Durability: the journal

Append-only events; the run context is a **fold** over `handler_finished` events. No Temporal-style
deterministic replay — side-effect *results* are journaled, so resume = fold + continue.

```
run_started      {machine_hash, input}
state_entered    {state, attempt}
handler_finished {state, output, event, usage{tokens, cost}}
transition_fired {from, to, on, guard}
run_parked       {state, reason: human_gate | shutdown, wake_token}
run_resumed      {event, payload}
run_finished     {terminal_state, status}
```

- Default store: **SQLite** via a pure-Go driver (`modernc.org/sqlite`, CGO-free) behind a
  small `Store` interface (Postgres later).
- `machine_hash` pins a run to the definition it started with; resuming under an edited
  YAML is an explicit `--migrate` decision, not an accident.
- The journal doubles as the **audit log**: every prompt, output, token count, and routing
  decision is reconstructable.

### Human gates

`human` states park the run (`run_parked` + wake token). v1 resume paths:

```
steps resume <run-id> --event approved --data '{"note": "ship it"}'
```

`timeout` + `on_timeout` route stale gates. Webhook resumption is a v1.x daemon concern.

## Guards: the Expr environment

All guards are compiled at load time against a typed environment derived from the state's
declared output schema — typos and type errors fail `steps validate`, not production.

| Symbol | Meaning |
|---|---|
| `ctx` | run context (namespaced per-state outputs + run input) |
| `output` | current state's validated output |
| `event` | agent-declared event (string, may be empty) |
| `attempt` | attempt number within current state |
| `state.elapsed` | duration in current state |
| `visits.<state>` | entry count per state this run — bound loops in guards (`visits.draft < 3`) |
| `run.transitions` | transitions so far |
| `run.tokens`, `run.cost` | cumulative usage |

Example: `when: output.confidence >= 0.8 && run.cost < 1.50 && attempt <= 2`

## Load-time validation (the guardrail payoff)

`steps validate workflow.yaml` — and the same checks at `Machine` build in Go:

- every state reachable from `initial`; a terminal state reachable from every state
- every non-terminal state ends in an unconditional fallback transition (the linear-flow
  default fills these in)
- every `on:` event is declared in that state's `output.events`
- every guard compiles, and references only fields that exist in the output schema / ctx shape
- every `{{ .ctx.X.Y }}` template resolves to a declared output of an upstream state or run input
- `limits.max_transitions` always in effect (engine default 50); models resolve to a registered provider; tools resolve to the registry

This is the differentiation vs. LangGraph-style code-wired graphs: because contracts are
declared, the machine is **statically checkable**.

## Go DSL sketch

YAML compiles to this; Go users can skip YAML entirely.

```go
m, err := steps.New("support-triage",
    steps.Limits(steps.MaxTransitions(25), steps.MaxCost(2.50), steps.Timeout(30*time.Minute)),
    steps.Initial("triage"),

    steps.State("triage",
        steps.Agent(
            steps.Model("anthropic/claude-haiku-4-5"),
            steps.System("You classify inbound support tickets."),
            steps.Tools(kbSearch),
            steps.MaxTurns(6),
        ),
        steps.In("ticket", "{{ .ctx.ticket.body }}"),
        steps.Out[TriageResult](steps.Events("resolved", "escalate")),
        steps.On("resolved").When(`output.confidence >= 0.8`).To("send_reply"),
        steps.On("escalate").When(`output.severity == "critical"`).To("page_oncall"),
        steps.Fallback("human_review"),
        steps.Retry(
            steps.Transient(5, steps.Backoff(time.Second, 2.0)),
            steps.Semantic(2),
        ),
    ),
    // ...
)

engine := steps.NewEngine(store, providers, registry)
run, err := engine.Start(ctx, m, steps.Input("ticket", ticket))
```

Key interfaces:

```go
// The pluggable state body. agent/llm/action/human all implement this.
type Handler interface {
    Run(ctx context.Context, in HandlerInput) (HandlerResult, error)
}
// HandlerResult{Output map[string]any, Event string, Usage Usage}

// Providers do ONE completion; the agent loop lives in the engine so
// retry/budget/journal semantics are identical across providers.
type Provider interface {
    Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
}

// Tools: Go funcs, schema derived by reflection from the params struct.
registry.Tool("kb.search", "Search the knowledge base",
    func(ctx context.Context, p SearchParams) (SearchResult, error) { ... })
```

## Testing strategy

Never assert LLM content; assert machine semantics. Three tiers:

1. **Mock provider (CI, fully deterministic)** — `provider/mock` plays scripted
   responses keyed by state name, including deliberate schema violations and injected
   provider errors, so retry/guard/journal behavior is asserted *exactly*:

   ```
   steps run workflow.yaml --input article=@fixtures/article.txt --mock mock_responses.yaml
   ```

   Tests assert the journal event sequence, retry counts, `visits` counters, terminal
   state, and file artifacts — no network, no nondeterminism.
2. **Live local (Ollama or any OpenAI-compatible endpoint)** — same YAML, real model,
   temperature 0. Assert invariants only: run reaches a terminal, loop bounds hold,
   every output validated against its schema. Never assert content.
3. **Durability drills** — `kill -9` the process mid-run, then `steps resume <run-id>`
   completes it from the journal; kill Ollama mid-run and watch transient retries and
   backoff appear as journal events.

The canonical example lives in [`examples/summarize-critic/`](examples/summarize-critic/)
and doubles as the acceptance spec for v1: every command in its README must work. Its
paired variant, [`examples/summarize-critic-adopt/`](examples/summarize-critic-adopt/),
is the same machine with `adopt: self` revisions and an identical mock script — running
both A/B-tests the two context philosophies (rung 1 `ctx` re-priming vs rung 3
conversation adoption) with the context mechanics as the only variable.

## Package layout (proposal)

```
steps/            # public API: Machine, State builders, Engine, Run
  yaml/           # YAML → Machine compiler + validator
  expr/           # guard compilation, typed env derivation
  journal/        # event types, Store interface, sqlite store
  provider/       # Provider iface, anthropic/, openaicompat/, mock/
  tool/           # registry, reflection schemas, builtin/ (http, json, file, ...)
  cmd/steps/      # CLI: run, resume, runs, validate, inspect
examples/         # runnable canonical examples; double as acceptance specs
```

## Prior art to steal from

- **Amazon States Language** — `Retry`/`Catch`/`Choice` semantics; closest YAML-ish precedent.
- **XState** — guard/action vocabulary; the statechart semantics to grow into (v1.x hierarchy).
- **state_machines (Ruby)** — the DSL ergonomics that inspired this; before/after transition callbacks are worth adopting as hooks.
- **LangGraph** — the closest competitor; differentiate on static validation, contracts, Go, durability-by-default.
- **Temporal** — what NOT to rebuild; we journal results instead of deterministic replay.

## Open questions

1. **Multi-provider scope** — is Anthropic + one OpenAI-compatible adapter (covers OpenAI,
   Ollama, vLLM, most gateways) sufficient for "multi-provider day one"? (2 adapters, not 4.)
2. **Tool-calling normalization** — the riskiest part of multi-provider: parallel tool calls,
   strict-JSON-schema support, and streaming differ per provider. Engine-owned agent loop
   mitigates; needs a conformance test suite per adapter.
3. **Observability** — journal is the audit log; do OTel spans + a lifecycle hook interface
   (`OnStateEnter`, `OnTransition`, …) belong in v1 or v1.x?
4. **Name** — "steps" collides conceptually with AWS Step Functions; fine, or worth renaming?
