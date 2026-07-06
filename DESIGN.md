# steps — a state-machine runtime for micro-agents

**Status:** v1 implemented, 2026-07-02 — see [README.md](README.md); examples pass as the acceptance suite
**Language:** Go · **Config:** TypeScript machines (esbuild → goja) — state consts + one flow expression; every computed value is a function of one flat scope
**Stack:** [google/adk-go](https://github.com/google/adk-go) v1 (agent loop, sessions, tools) + [adk-utils-go](https://github.com/achetronic/adk-utils-go) (Anthropic + OpenAI-compatible clients) · [esbuild](https://github.com/evanw/esbuild) (TS transpile)

> **2026-07 config-language revision (two rounds):** machines were originally
> YAML with Go templates and Expr guards embedded in strings — two languages
> smuggled inside a third. Round one replaced YAML with JS data. Round two
> (owner: "trivial must be trivial; the user must be FORCED into states")
> settled the final shape: **state consts + one `flow` expression**
> (pipe/branch/when — the graph visible in one place, compiled into the same
> enforced transitions), and every computed value is a function of ONE FLAT
> scope, destructured by name (`({article, critique}) => ...`) with native
> `${}` interpolation. The destructured parameter list doubles as the
> state's declared input contract, checked at load. Enforcement is
> unchanged: combinators build the graph, never bypass it.

> Implementation deltas from this document: the Go builder DSL (`steps.State(...)`)
> is not yet exposed — Go users construct machines via `machine.Parse`; `tool_choice:
> required/one_of` validates as "not implemented in v1"; `history` renders
> messages/tool_calls/thoughts with `last_turns` but no per-turn pairing; costs are
> tracked but no per-model pricing table exists yet (token budgets work). Webhook
> *triggering* shipped ahead of this document: a machine may declare a `webhook:
> {path, map}` block (map is a function of `{body, headers, query, ...hook inputs}`
> returning run inputs, dry-run at load), and `steps serve --hook wf.ts` exposes
> `POST /hooks/<path>` to start a run — trigger-only; webhook *resumption* of a
> parked gate remains v1.x. `http.get` gained an optional `headers:` arg.
>
> Token-discipline additions beyond this document (all measured on live runs):
> `agent.max_output_tokens` (default 2048 — no state may generate unboundedly;
> cap exhaustion classifies as `budget_exceeded`, never retried); `agent.reasoning:
> low|medium|high` (maps to provider reasoning effort; per-micro-agent thinking
> budgets); `agent.structured_output: prompt|native` (native = decoder-constrained
> JSON on OpenAI-compatible backends; opt-in because grammar sampling degenerates on
> some local model/backend combos); `adopt: {from, lastTurns}` trim; reasoning-
> channel messages are journaled flagged `thought` but NEVER replayed on adopt and
> excluded from `history` unless `include: [thoughts]` — scratch thinking is not
> context. `steps inspect <run-id>` renders per-state token usage and routing.
> `distill:` (rung 1.5, [docs/distill.md](docs/distill.md)): a state declares
> what it needs from a large scope value; a cheap model extracts only that
> slice via a lowered implicit state (`name#key`) — journaled, memoized,
> budgeted — and inside the state the declared key IS the slice.
> `agent.maxInputTokens` (default 8192 — the input mirror of the output cap;
> the rendered system+prompt is estimated at chars/4 before any model call,
> and overflow is a zero-token `budget_exceeded` naming the largest
> destructured inputs; `0` opts out; implicit distill states are exempt).
> `maxTurns` defaults conditionally: 2 for tool-less states, 10 with tools.
> The `loop()` combinator (see flow combinators below) packages the bounded
> judge/revise cycle every example was hand-writing as a raw branch.

## Thesis

LLM agents need structure, not vibes. A finite state machine gives you:

- **Legibility** — the entire possible behavior of the system is enumerable at load time.
- **Guardrails** — transitions are the only way to move; guards are the only way to transition.
- **Micro-agents** — each state is a hyper-specific task with its own model, tools, budget, and
  contract, the way a microservice owns one job.
- **Operational semantics** — retries, timeouts, budgets, and exits are properties of the
  machine, not conventions inside prompts.

## Design principle: optimize for the human

The YAML (and the Go DSL under it) is read far more often than it is written,
and every mistake that survives to runtime wastes tokens. So the DSL is held
to three rules, in priority order:

1. **Fail before you spend.** Anything checkable at load time IS checked at
   load time, with an error that names the state, the field, and the valid
   options. Opaque functions are no excuse: `steps validate` **dry-runs every
   function** against stub scopes derived from the declared schemas, so
   `output.sevrity` fails the load naming the misspelled field *and* the
   available ones. Declaring `input:` buys strict checking of run inputs too.
2. **One honest language, nothing smuggled into strings.** Structure is data;
   logic is a plain JS function of one scope argument. Interpolation is a
   template literal, a predicate is `.every(...)`, a join is `.find(...)` —
   never a mini-language inside a string. Schema shorthand
   (`risk: "enum(low, medium, high)"`, `leads: [{where: "string"}]`,
   `tags: "string[]"`) stays because it is data, not code.
3. **The truth is one command away.** `steps validate --print` shows the
   machine after defaults; `steps context` shows what each state's functions
   may reference; `steps inspect` shows what a run actually did. And
   `docs/src/global.d.ts` gives editors autocomplete over the whole DSL.

## Decisions (locked)

| Question | Decision |
|---|---|
| Transition driver | **Agent proposes, guards dispose** — the agent declares an event; the flow expression (pipe/branch/when) compiles to ordered transitions matching event AND guard; first match wins; mandatory fallback |
| State body | **Pluggable handlers** — `agent` (LLM+tools loop), `llm` (single call), `action` (registered Go func), `human` (gate) |
| Data flow | **Explicit contracts** — templated `input:` pulled from run context, typed `output:` schema merged back as `ctx.<state>.*`; fresh conversation per state; declared escape hatches only (`history` projection, `adopt` continuation) |
| Durability | **Durable from day one** — event-sourced journal; runs survive crashes and park on human gates |
| Providers | **Multi-provider from day one** — thin owned interface; Anthropic + OpenAI-compatible adapters (`openai`, `ollama`, `lmstudio`) + `openrouter` (OpenAI-compatible plus OpenRouter's prompt-caching surface on a scoped HTTP client) |
| Tools | **Registered Go functions** (reflection-derived schemas) + **built-in tool library** (http, json, exec…) |
| Packaging | **Library + CLI** — `steps run`, `steps resume`, `steps runs`, `steps validate` |
| Graph model | **Flat FSM + `foreach` fan-out** — states may map their handler over a ctx list (sequential v1; hermetic per-item contexts); sub-machines and parallel regions remain v1.x |
| Defaults | **Convention over configuration** — linear flow by declaration order when no `flow:` given, implicit terminals, default retry/limits; handler inferred from state keys |
| Testing | **Mock provider in CI** (scripted responses, fully deterministic), live iteration via Ollama, durability drills; `examples/` double as the acceptance spec |

## Core nouns

- **Machine** — the workflow definition (states + transitions + limits). Immutable, validated at load.
- **State** — one handler + an input contract + an output contract + transitions + retry/catch policy.
- **Transition** — `{on?, when?, to}`. `on` matches the agent-declared event; `when` is a
  guard function. Both optional; both must hold if present. Evaluated in order; a bare `{to}` is the fallback.
- **Run** — one durable execution instance: journal + context fold + current position.
- **Handler** — the pluggable state body (`agent`, `llm`, `action`, `human`).
- **Guard** — a plain function of scope returning bool; dry-run at load against schema-derived stubs.
- **Journal** — append-only event log; the run's source of truth.

## The machine format

A machine is one `workflow.ts` file: plain state consts, a `states:` map that
names them, and a `flow` expression that IS the topology. Any computed value
is a function of one flat scope — destructure what you need; the parameter
list is the state's declared contract.

Machines are **TypeScript**, not raw JS: `machine.Load` transpiles them with
[esbuild](https://github.com/evanw/esbuild) (in-process, no Node) — types are
stripped, `export default` lowers to CommonJS — then runs the result on goja.
So machines type-check in any editor: each file opens with
`/// <reference path=".../docs/src/global.d.ts" />`, and annotating state
consts `: State` (plus an optional `satisfies Machine` on the export) turns on
full key/value autocomplete and structural checking — `tsc --noEmit` passes on
the examples. Types are an authoring aid only; the runtime dry-run is what
verifies a destructured name against a state's *actual* scope.

```ts
const triage = {
  system: "You classify inbound support tickets.",
  tools: ["kb.search"],
  prompt: ({ ticket, fetch_history }) => `
    TICKET: ${ticket.body}
    HISTORY: ${fetch_history.summary}`,
  output: {
    severity: "enum(low, high, critical)",
    confidence: "number",
    reply_draft: "string",
  },
  events: ["resolved", "escalate"], // injected into the schema as an enum
};

const fetch_history = {
  action: "crm.fetch_history", // registered Go func
  input: ({ ticket }) => ({ customer_id: ticket.customer_id }),
};

const human_review = {
  human: ({ triage }) => `Agent unsure (confidence ${triage.confidence}). Approve draft?`,
  timeout: "24h",
};

const send_reply = {
  action: "mail.send",
  input: ({ ticket, triage }) => ({ to: ticket.customer_email, body: triage.reply_draft }),
};

const page_oncall = {
  action: "pagerduty.page",
  input: ({ triage }) => ({ summary: triage.reply_draft }),
};

export default {
  name: "support-triage",
  input: { ticket: { type: "object", required: true } },
  models: { triager: "anthropic/claude-haiku-4-5" },
  model: "triager",
  defaults: { reasoning: "low" },
  limits: { maxTransitions: 25, maxCost: 2.5, timeout: "30m" },

  states: { fetch_history, triage, human_review, send_reply, page_oncall },

  flow: pipe(
    branch(fetch_history, { catch: { action_error: fail } }),
    branch(triage, {
      resolved: when(({ output }) => output.confidence >= 0.8).to(send_reply),
      escalate: when(({ output }) => output.severity === "critical").to(page_oncall),
      else: branch(human_review, {
        approved: send_reply,
        rejected: triage, // loop back; maxTransitions bounds it
        timeout: page_oncall,
      }),
    }),
  ),
};
```

### The flow combinators (the graph in one expression)

- `pipe(...steps)` — sequence; each step falls through to its successor.
- `branch(state, { event: target, else: target, catch: {class: target},
  timeout: target })` — ALL outgoing edges of a state, in order. Event keys
  must be declared in `events:`; `else` is the fallback (mid-pipe, the pipe
  successor is the implicit else; human gates need none — their resume
  events are the complete alphabet); `catch` routes error classes; `timeout`
  routes an expired gate. Array form for guard-only edges:
  `branch(s, [when(g).to(a), fallback])`.
- `when(fn).to(target)` — a guarded edge (`.to`, not `.then` — thenables are
  a Promise footgun). Targets are state consts, nested `pipe`/`branch`/`loop`,
  or the terminals `done`/`fail` — a typo'd const is a native ReferenceError.
- `loop(body, { judge, accept, maxVisits, then?, revise?, exhausted?,
  catch? })` — the bounded judge/revise cycle, the shape every example was
  hand-writing as a raw branch. The body falls through to the judge; the
  judge gets exactly `[accept → then, visits.<judge> < maxVisits → revise,
  fallback → exhausted]`. Guard-only — the combinator never touches the
  judge's `output`/`events` (flow shapes topology, states own contracts),
  which is what lets an *action* be a judge: a build command's exit code
  routes via `accept: ({ output }) => output.ok`. The bound counts the
  judge's own visits — the gate that observes the loop — synthesized as real
  JS so it dry-runs, contract-checks, and `--print`s like a hand-written
  guard. `maxVisits` is required (the declared bound is the point); `then`
  defaults to the pipe successor, `revise` to the body's entry (explicit for
  loops that re-enter upstream, e.g. a build loop that resubmits the coder),
  `exhausted` to `fail`. Event-conjunction stays expressible in the guard:
  `accept: ({ event, output }) => event === "approve" && output.score >= 8`.
  A gate that never loops is just a `branch`; self-judging states (judge ==
  body) are hand-written array-form branches in v1. A judge that declares
  `verdict:` needs no `accept:` — the state's own acceptance test IS the
  accept edge, so the criterion is not restated across the output schema, an
  `events:` list, and a guard.
- `gate(name, { prompt, approve | choices, timeout?, onTimeout? })` — the
  human-escalation counterpart of `loop()`: it synthesizes a real human state
  (`gate#name`, the same `owner#key` move as `distill:`) and its branch tail.
  `approve` routes to a target with a synthesized `rejected`/`timeout → fail`;
  `choices: {event: target | {to, label}}` is the full form. Usable anywhere a
  target is (a loop's `exhausted:`, a branch edge, mid-pipe). `gate` is a
  shadowable global, so a state literally named `gate` still works — only calls
  reach the combinator.
- A non-terminal state with no wiring anywhere flows to `done`. Without a
  `flow:`, linear declaration order applies — trivial machines need none.
- The combinators COMPILE INTO the per-state transition lists the engine has
  always enforced. Reachability, terminal proofs, fallback presence, event
  declarations — all still fail the load. Sugar, never a bypass.

### The flat scope (the whole vocabulary)

Every function receives one destructurable object; the parameter list is a
statically checked contract (`({critque}) =>` fails the load naming the
available keys, when `input:` is declared):

| Key | Meaning |
|---|---|
| `<input>` | each declared run input, by name |
| `<state>` | each prior state's output, by name |
| `<as>`, `index`, `total` | the forEach item and its position |
| `output`, `event` | this state's result (flow guards only) |
| `visits` | entry counts — bound loops (`visits.draft < 3`) |
| `run` | `{transitions, tokens, cost}` cumulative |
| `attempt` | attempt number within the current state |
| `args`, `calls`, `turn` | tool guards: model-authored args, per-tool counts, turn |

Prompt, system, and human strings are auto-dedented (common leading
whitespace stripped) so machines indent naturally; file `content` is written
verbatim — whitespace matters when writing files. Host helpers: `list(xs)` renders
bulleted lines, `yaml(v)` compact YAML, `include(path)` reads a pinned
prompt file.

## Defaults — convention over configuration

Every key except `states` and each state's handler is optional. Defaults are applied
*before* validation, so the machine that runs is always fully explicit —
`steps validate --print` shows the expanded form.

**Flow**
- `initial` → the first state declared (JS object insertion order is preserved).
- A state with no `transitions:` → one transition to the next declared state;
  the last state → `done`. Linear pipelines need zero transition blocks.
- `done` and `failed` terminals always exist implicitly; declare them only to customize.
- No `catch:` → unhandled exhaustion routes to `failed` with the reason journaled.

**Agent states**
- `model`, `temperature` (0), `max_turns` (2 tool-less / 10 with tools),
  `max_output_tokens` (2048), `max_input_tokens` (8192; `0` = off; distill
  states exempt), `structured_output` (prompt), `reasoning` (provider
  default) cascade: state → machine `defaults.agent` → engine option →
  validation error only if still unresolved. `max_turns` bounds model calls
  per conversation turn and resets across semantic retries.
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

The payoff — this is a complete, valid machine (no flow: linear declaration
order; bare-string state = agent prompt):

```ts
export default {
  name: "summarize",
  states: {
    draft: "Summarize the article in 3 bullets",
    publish: { write: "out/summary.md", content: ({ draft }) => draft.text },
  },
};
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

```ts
const triage = {
  maxTurns: 6,
  tools: [
    "kb.search", // bare: unrestricted
    {
      name: "crm.refund",
      maxCalls: 1, // per-tool budget within this state
      when: ({ args, run }) => args.amount <= 500 && run.cost < 1.0,
      onReject: "feedback", // feedback (default) | fail
    },
    { name: "mail.send", require: "kb.search" }, // only after a search
  ],
  prompt: ({ ticket }) => `...${ticket.body}`,
};
```

- **Tool guards** are functions evaluated at call time. Their scope adds `args`
  (the model-authored arguments — the thing being judged), `calls.<tool>`
  (invocation counts this state), and `turn`, alongside the usual `ctx`/`run`.
- **`on_reject: feedback`** (default): the guard failure is returned to the model as
  the tool result, so it adapts within the loop — the same retry-with-feedback shape as
  schema violations, bounded by `max_turns`. **`on_reject: fail`** surfaces a semantic
  failure to the state's retry/catch instead.
- **`max_calls`** and **`require`** give the loop budget and ordering constraints
  without promoting every tool call to a full state.
- **`args`** pins machine-authored args (an object or a function of scope)
  merged over the model's args at execution — repo roots, tenant IDs,
  credentials-by-ref. The model never sees them and cannot override them:
  the model chooses *when*, the machine chooses *where*.
- Every proposed call, guard verdict, and rejection feedback is journaled — the
  interior is stochastic but fully audited.

### The dispatch step

`tool_choice: one_of` — the state completes after exactly one tool call:

```ts
const route = {
  toolChoice: "one_of", // auto (default) | required | one_of
  tools: ["billing.lookup", "shipping.trace", "kb.search"],
};
```

Choosing a tool *is* proposing an event, so in `one_of` mode the chosen tool's name
becomes the state's event and ordinary transitions guard it
(`on: billing.lookup, when: ..., to: ...`). This is the LLM as a typed router:
nondeterministic in *which* branch, deterministic in *shape* — exactly one call,
schema-validated args, guarded routing. It fills the middle ground between `action`
(no chooser) and a free agent loop.

## Fan-out: foreach

A state may map its handler over a list evaluated from ctx:

```ts
const scout_files = {
  forEach: {
    over: ({ split_diff }) => split_diff.files, // function of scope returning the list
    as: "file",                                 // the item's scope name
  },
  prompt: ({ file }) => `What deserves a senior's attention in ${file.path}? ...`,
  output: { path: "string", risk: "enum(low, medium, high)" }, // PER-ITEM shape
};
```

- Each item is **hermetic**: agents get a fresh conversation per item — N
  small context windows instead of one big one. This is the context thesis
  applied to scale: the machine assembles per-item context; no item sees its
  siblings.
- The state's ctx entry becomes `{items: [...], count: n}` (plus
  `skipped`/`failures` under `onItemFailure: "skip"`, and `memo_hits` when
  memoized items replayed); downstream guards and `over` functions compose
  with plain JS: `ctx.scout_files.items.filter(i => i.risk !== "low")`.
- Items share the state's retry policy; templates also see `.index`/`.total`.
- `concurrency` bounds parallel items (default 1; mock runs force
  sequential so scripted queues stay deterministic).
- `onItemFailure: "skip"` drops poisoned items instead of failing the state;
  guards react via `output.skipped`.
- `carry: true` makes each `items` entry `{item, output, index}` — pairing
  every output with its source item and its position in the original `over`
  list. This is the skip-safe alternative to zipping a parallel list back by
  index (`plan.files[i]`): once `skip` drops an entry the hand zip misaligns,
  but the engine knows the true pairing, so `items.map(e => e.item.path)` stays
  correct.
- foreach states cannot declare events (no single event exists — route with
  guards over the aggregate), cannot adopt/history, and cannot wrap human gates.

## Spend controls: memo, routing, budgets

- **Memoization** (`memo: true`, agent states only): outputs are cached in
  the journal store keyed on hash(model + rendered system + rendered prompt).
  Byte-identical input replays the cached output for zero tokens — re-review
  a PR and only changed files re-pay. Actions never memoize: side effects
  must not be skipped.
- **Dynamic model routing** (`model` as a function of scope):
  `({ lead }) => lead.risk === "high" ? "senior" : "scout"`. Dry-run at
  load; the result must be a `models:` alias or ref.
- **`models:` aliases** name capabilities, not vendors: states say `senior`,
  the header says what senior means today. An alias may be a **tier** —
  `senior: { model: "…", reasoning: "high", maxOutputTokens: 8192, memo: true }`
  — bundling the per-role knobs so "cheap scout vs expensive senior" is declared
  once and states just select it with `model: "senior"`. Precedence:
  state-explicit > tier > `defaults:` > engine default (an explicit
  `memo: false` beats the tier). A tier named by a dynamic routing function
  contributes only its ref — the knobs are load-time.

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

```ts
const verify = {
  history: {
    from: "research",
    include: ["tool_calls"], // messages | tool_calls | thoughts
    lastTurns: 10,
    as: "trace",
  },
  prompt: ({ trace }) => `
    Did the researcher actually consult real sources?
    ${trace}`,
};
```

Hermetic in mechanism: the record crosses as declared input text, statically validated
(`from:` must be a graph-predecessor), journaled like any other input. History is data
*about* a conversation, not the conversation.

**Rung 3 — `adopt`: *becoming* a state's continuation.** The state receives the prior
state's actual normalized message array with its own prompt appended.

```ts
const take_over = {
  model: "anthropic/claude-opus-4-8", // tier escalation: opus resumes where haiku stalled
  adopt: "triage",
  prompt: "The previous agent could not resolve this. Take over.",
};
```

Earned by two cases: **tier escalation** (tool results already in the transcript are
not re-executed — they may be non-idempotent and they cost money) and **revision
loops** via `adopt: "self"` (on revisit, continue your own prior conversation instead of
being re-primed). This rung genuinely breaks hermeticity — explicitly, one declared
edge at a time.

Mechanics and rules:

- The journal already stores each state execution's normalized messages (audit,
  resume), so `history` is a render of the journal and `adopt` is a replay of it — no
  new storage, and both survive crash/resume.
- `history.from` / `adopt` targets must be graph-predecessors (load-time check).
  Adopting a state that never executed on this run's path is a semantic failure routed
  to `catch:` — never a silent fresh start. `history` of a never-executed state renders
  empty with a journaled warning. Exception: `adopt: "self"` on a state's *first* visit
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

## Load-time validation (the guardrail payoff)

`steps validate machine.ts` — and the same checks at `machine.Load` in Go:

- every state reachable from `initial`; a terminal state reachable from every state
- every non-terminal state ends in an unconditional fallback transition (the linear-flow
  default fills these in)
- every `on:` event is declared in that state's `output.events`
- **every function is dry-run** against schema-derived proxy stubs: accessing a
  field that cannot exist fails the load, naming the function, the field, and
  the available fields; infinite loops surface as warnings (1s interrupt)
- `limits.maxTransitions` always in effect (engine default 50); model aliases resolve;
  tools resolve to the registry

This is the differentiation vs. code-wired graphs: because contracts are
declared and the machine is data, even the opaque functions are checkable
before a single token is spent.

## Go embedding

The engine is a library first: `machine.Load`/`machine.Parse` build machines
from JS source, `engine.New(store, providers, registry, listener)` runs them,
and `toolreg.Register` exposes Go functions to machines. A fluent Go builder
(`steps.State(...)`) remains future work — with logic living in the machine's
own JS, the builder's value is mostly programmatic generation.

```go
m, err := machine.Load("review.js")
engine := engine.New(store, provider.NewRegistry(), registry, listener)
res, err := engine.Start(ctx, m, map[string]any{"diff": diff})

// Tools: Go funcs the machine can name in action:/tools:.
registry.Register("kb.search", "Search the knowledge base",
    func(ctx context.Context, args map[string]any) (map[string]any, error) { ... })
```

## Testing strategy

Never assert LLM content; assert machine semantics. Three tiers:

1. **Mock provider (CI, fully deterministic)** — `provider/mock` plays scripted
   responses keyed by state name, including deliberate schema violations and injected
   provider errors, so retry/guard/journal behavior is asserted *exactly*:

   ```
   steps run workflow.ts --input article=@fixtures/article.txt --mock mock_responses.yaml
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
machine/          # JS loader (goja), Dyn values, defaults, schemas, validation, dry-run
journal/          # event types, Store interface, sqlite store, fold
engine/           # run loop, retries, budgets, handlers (agent via ADK, action, human)
provider/         # model-ref registry, mock provider, error classification
toolreg/          # named Go functions + builtins (file, diff, http, gh, exec)
docs/src/         # global.d.ts — ambient TypeScript types for machine files
cmd/steps/        # CLI: run, resume, runs, validate, context, inspect
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
