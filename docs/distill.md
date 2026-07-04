# distill — declared context slicing

**Status:** implemented (2026-07-04) — `machine/distill.go` is the lowering,
`engine.applyDistill` the scope mapping; acceptance coverage in
`machine/distill_test.go` and `engine/distill_test.go`. The companion knob
`maxInputTokens` is **not yet implemented** (needs a pre-call token
estimator; see Implementation deltas below).
**Position in the rung ladder:** rung 1.5 — between "what upstream *concluded*"
(rung 1 `ctx`) and "what upstream *did*" (rung 2 `history`).

> **Implementation deltas from this sketch:**
> - `maxInputTokens` is deferred — enforcement before the call needs a token
>   estimator the engine doesn't have yet; distill stands alone.
> - `forEach.over` and flow guards see the **pre-distill** scope: `over` must
>   produce the same list for the implicit fan-out and the consumer (zip by
>   index), and guards run at the boundary, not inside the state.
> - The dry-run's shadowed-key stub permits string methods (`spec.trim()`)
>   and interpolation; any other field access fails the load naming the
>   distillation.
> - Distill hops don't count toward `limits.maxTransitions` and their
>   `transition_fired` events carry `implicit: true` (the fold skips them).
> - **Absent source = empty slice, for free.** A source state that has not
>   executed on this run's path (loop feedback before the loop — `build`
>   stderr on the coder's first visit) yields `""` with no model call, like
>   `adopt: "self"` on a first visit. The consumer's ternary
>   (`${build_cause ? ... : ""}`) reads it as falsy.
> - User state names are now validated as JS identifiers, which is what keeps
>   the lowered `name#key` namespace collision-free.

## Problem: payload copying

The rung system solved *history* copying; it did not solve *payload* copying.
In [`examples/codegen/workflow.ts`](../examples/codegen/workflow.ts), `${spec}`
is interpolated verbatim into `plan`, `generate`, and `review` — and `generate`
is a forEach, so the whole spec rides in **every per-file context, on every
revision loop**. Same story for `build.stderr`: a 200-line compiler dump is
pasted raw into each coder retry when three lines of root cause would do.

The author has no cheaper declared alternative today. Either upstream
hand-shapes an output field for every downstream consumer (more human code),
or you paste the payload (more tokens, less isolation). Rung 1's advice —
"make upstream declare it as an output field" — assumes upstream can predict
every consumer's needs. A forEach consumer proves it can't: the slice of the
spec that matters is a function of *the item*, which upstream never sees.

`maxOutputTokens` is defaulted and enforced; the input side of a state is
unbounded and unmeasured. The context thesis is enforced on one side of the
boundary only.

## The feature in one example

```ts
const generate: State = {
  forEach: { over: ({ plan }) => plan.files, as: "target", concurrency: 3 },
  memo: true,
  model: "coder",

  distill: {
    // Shadow `spec`: inside this state, `spec` IS the slice.
    spec: {
      for: ({ target }) => `only what is needed to implement ${target.path} (${target.purpose})`,
      maxTokens: 400,
    },
    // New name from a source: `build` stays visible, `build_cause` is derived.
    build_cause: {
      from: "build",
      for: "the root-cause error(s) only — exact messages, file:line, nothing else",
      maxTokens: 200,
    },
  },

  prompt: ({ spec, target, build_cause, review }) => `
    Write the COMPLETE contents of exactly one file...
    FILE: ${target.path}
    SPEC (relevant slice):
    ${spec}
    ${build_cause ? "The build failed. Root cause:\n" + build_cause : ""}`,
};
```

Two distinct wins in one state: the per-file spec slice (a *function of the
item* — upstream could never have declared it), and the stderr root-cause (a
compression no human wants to hand-maintain).

## Surface

```ts
/** State: replace (or derive from) a large scope value with a model-extracted slice. */
distill?: Record<string, Distill>;

interface Distill {
  /** What this state needs from the source. String or function of scope. Required. */
  for: string | Fn<string>;
  /** Source scope key (run input or predecessor output). Default: this entry's key — shadowing. */
  from?: string;
  /** Output budget of the slice = maxOutputTokens of the implicit state. Default 512. */
  maxTokens?: number;
  /** Alias/ref. Default resolution: models.distiller → machine default model. */
  model?: string;
  /** Replay by hash(model + source + need). Default TRUE (opt out per entry). */
  memo?: boolean;
}
```

- **Key = shadow** (`spec:` above): inside this state's functions, `spec` is
  the distilled *string*. The original is not reachable — that is the point.
  The dry-run stubs reflect the shadow, so `spec.title` on a shadowed key
  fails the load naming the distillation.
- **`from:` = derive**: introduces a new name; the source stays visible. Use
  when a function genuinely needs both.
- The distilled value is always a plain string (the engine's raw-`{text}`
  lesson: never wrap a payload in a JSON envelope). Non-string sources render
  through `yaml()` before distillation.
- Legal on agent, action, and human states (it is scope shaping, not a
  handler concern). A human gate summarizing a huge stderr for the operator
  is a first-class use. Illegal on terminals.

## Lowering: sugar → real states

Per the locked invariant — *one state = one handler invocation = one journal
entry = one retry policy* — `distill` compiles into implicit agent states in
the defaults-expansion pass, exactly the move reserved for `action: [a,b,c]`:

1. Each entry on state `s` lowers to an implicit state named **`s#key`**
   (e.g. `generate#spec`). `#` cannot appear in a JS identifier, so implicit
   states can never collide with user consts and their outputs can never be
   destructured by other states — inaccessibility is structural, not policed.
   (Validation gains the rule: user state names must be valid identifiers.)
2. **Graph rewrite:** every transition `→ s` retargets to `→ s#key1`; the
   implicit states chain (`s#key1 → s#key2 → … → s`). Guards evaluate at
   their original position; the chain's own edges are unconditional. If `s`
   was `initial`, initial moves to the head of the chain. Loop-backs pass
   through the chain too — deliberate: on a revisit the *need* may have
   changed (new review feedback), and memo makes unchanged pairs free.
3. **Implicit-state defaults:** `memo: true`, `temperature: 0`,
   `reasoning: "low"`, `maxTurns: 1`, `maxOutputTokens: entry.maxTokens`,
   output `{text: string}`, no events, no tools. The slice budget IS the
   existing output-cap enforcement — no new mechanism.
4. **forEach inheritance:** when the consumer declares `forEach`, each
   implicit state inherits `over`, `as`, and `concurrency` (not
   `onItemFailure` — v1 pins `fail`; see open questions). The consumer's
   per-item scope maps `key` to `s#key.items[index].text`, aligned by index —
   the same zip-by-index contract `write_files` already relies on.
5. **Failures are the consumer's failures:** the consumer's `catch:` edges
   are copied onto each implicit state, and semantic/transient retries follow
   the consumer's policy. Authoring model: "my distillation failing is me
   failing."
6. **Accounting:** implicit transitions do NOT count toward
   `limits.maxTransitions` (loop bounds stay properties of the *authored*
   topology); tokens and cost count normally — `steps inspect` shows `s#key`
   indented under `s`.

`steps validate --print` shows the lowered machine; the sugar is fully
inspectable. The mock provider keys scripts by state name as always —
`generate#spec:` entries make CI deterministic with zero new mock surface.

## The distiller prompt (engine-owned)

One fixed system prompt, not user-templatable in v1:

> From SOURCE, extract only what is relevant to NEED. Prefer verbatim quotes.
> Preserve identifiers, signatures, paths, numbers, and error text exactly.
> No commentary, no restructuring, no invention. If nothing is relevant, say
> only: `(nothing relevant)`.

The user message is `NEED: <rendered for> \n SOURCE: <rendered from>`. This
is an *extractor*, not a summarizer — verbatim-quote bias is what makes a
0.6B–8B local model trustworthy at the job. The `for:` string is the only
prompt-shaping knob; if that proves insufficient the escape hatch already
exists (write a real state), which is the correct pressure.

## Defaults (the local-first path)

- Model resolution: `entry.model` → `models.distiller` alias → machine
  default model. `steps validate` emits a note when distill is used with no
  `distiller` alias — the recommended shape is a cheap local ref
  (`lmstudio/…`, `ollama/…`). When model ladders land, `distiller` is the
  alias most obviously served by `["lmstudio/…", "openrouter/…"]`.
- `memo: true` by default: distillation is pure (no side effects, no tools),
  so replay is always safe — the same argument that forbids memo on actions
  permits it here unconditionally.
- Memo cascade: the consumer's memo hash covers its rendered prompt, which
  contains the distilled text — so a stable distillation (memo hit) feeds a
  stable consumer hash. Unchanged (file, feedback) pairs stay free end to
  end across build loops.

## Companion knob: `maxInputTokens`

The mirror of `maxOutputTokens`, and what makes the thesis *enforced* rather
than aspirational:

- `maxInputTokens` on a state (cascade: state → `defaults.maxInputTokens` →
  engine default — proposed default generous, 16k, so existing machines keep
  loading; tighten per state).
- Rendered prompt over budget → `budget_exceeded` (exhaustion class: never
  retried, routable by `catch:`). The natural fix at the callsite is a
  `distill:` entry — the error message says so, naming the largest scope
  values in the rendered prompt.
- `steps validate` warns when schema stubs make overflow statically plausible;
  `steps inspect` grows an input-tokens column (the context bill) next to the
  output column it already has.
- The distiller states themselves get `maxInputTokens: unlimited` — they are
  the one place the big payload is *supposed* to appear. A source exceeding
  the distiller model's context window is a hard, named load-or-run error
  (`distill source ~Nk tokens exceeds distiller context; split upstream or
  pick a larger distiller`); map-reduce chunked distillation is explicitly
  v1.x.

## Validation (fail before you spend)

- `from` (or the shadowed key) must resolve to a declared run input or a
  graph-predecessor state's output — the same reachability check as
  `history.from`. Engine-supplied keys (`visits`, `run`, `output`, …) are
  not distillable.
- `for:` functions dry-run against schema stubs like every other function.
- Shadowing rewrites the consumer's stub: the shadowed key becomes a string
  stub, so field access on it fails the load with "distilled to text by
  `distill.spec`" in the message.
- Distill entries are independent: every `for:` sees the *pre-distill* scope
  (no entry may reference another's output; chain real states if you need
  staged compression).
- `maxTokens` must be < the consumer's `maxInputTokens` (when both set) —
  a slice that cannot fit its consumer is a load error, not a runtime one.

## What proves it (measurement, not vibes)

Same discipline as the adopt/thought-exclusion numbers in DESIGN.md: run
`examples/codegen` live through a 3-iteration build loop, before and after
adding the two distill entries above, and compare `steps inspect` totals —
per-visit input tokens to `generate` items, and end-to-end cost. The claim to
beat: per-file input cost should drop from O(spec) to O(slice) after visit 1,
and re-visits with unchanged files should show `memo_hits` on both `generate`
AND `generate#spec`. If the distilled runs don't converge as reliably (slices
lost something the coder needed), that is a finding about `for:` authoring,
and it must go in the example README either way.

## Open questions

1. **forEach `onItemFailure: skip` alignment** — if the consumer skips
   poisoned items, should a failed distillation skip the item too? v1 pins
   distill fan-outs to `fail` (routed by the consumer's `catch:`); revisit
   when a real machine hits it.
2. **Faithfulness checks** — a `check: (scope) => boolean` guard on the
   slice (e.g. "must contain the function signature") would catch lossy
   distillation at the boundary. Deferred: it's the tool-guard shape applied
   to distill, wait for evidence it's needed.
3. **Cross-entry staging** — distilling a distillation (map-reduce over huge
   sources). Deliberately excluded from v1; the flat-FSM answer is "write a
   real state," and that answer might just be correct.
4. **Should `maxInputTokens` default tighter than 16k?** Tighter default =
   stronger thesis, more migration friction for existing machines. Decide
   after the context-bill column exists and real machines show their sizes.
