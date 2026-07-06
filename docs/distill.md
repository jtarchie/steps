# distill — declared context slicing

`distill:` declares what a state needs from a large scope value, and a cheap
model extracts just that slice before the state runs — so the whole payload
never enters the context. It's rung 1.5 of the context ladder: between what
upstream _concluded_ (rung 1, `ctx`) and what upstream _did_ (rung 2,
`history`). Everything here lowers to real states you can see with
`steps validate --print` — the design record is at the [bottom](#under-the-hood).

## Problem: payload copying

The rung system solved _history_ copying; it did not solve _payload_ copying. In
[`examples/codegen/workflow.ts`](../examples/codegen/workflow.ts), `${spec}` is
interpolated verbatim into `plan`, `generate`, and `review` — and `generate` is
a forEach, so the whole spec rides in **every per-file context, on every
revision loop**. Same story for `build.stderr`: a 200-line compiler dump is
pasted raw into each coder retry when three lines of root cause would do.

The author has no cheaper declared alternative today. Either upstream
hand-shapes an output field for every downstream consumer (more human code), or
you paste the payload (more tokens, less isolation). Rung 1's advice — "make
upstream declare it as an output field" — assumes upstream can predict every
consumer's needs. A forEach consumer proves it can't: the slice of the spec that
matters is a function of _the item_, which upstream never sees.

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
      for: ({ target }) =>
        `only what is needed to implement ${target.path} (${target.purpose})`,
      maxTokens: 400,
    },
    // New name from a source: `build` stays visible, `build_cause` is derived.
    build_cause: {
      from: "build",
      for:
        "the root-cause error(s) only — exact messages, file:line, nothing else",
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

Two distinct wins in one state: the per-file spec slice (a _function of the
item_ — upstream could never have declared it), and the stderr root-cause (a
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

- **Key = shadow** (`spec:` above): inside this state's functions, `spec` is the
  distilled _string_. The original is not reachable — that is the point. The
  dry-run stubs reflect the shadow, so `spec.title` on a shadowed key fails the
  load naming the distillation.
- **`from:` = derive**: introduces a new name; the source stays visible. Use
  when a function genuinely needs both.
- The distilled value is always a plain string (the engine's raw-`{text}`
  lesson: never wrap a payload in a JSON envelope). Non-string sources render
  through `yaml()` before distillation.
- Legal on agent, action, and human states (it is scope shaping, not a handler
  concern). A human gate summarizing a huge stderr for the operator is a
  first-class use. Illegal on terminals.

## Lowering: sugar → real states

Per the locked invariant — _one state = one handler invocation = one journal
entry = one retry policy_ — `distill` compiles into implicit agent states in the
defaults-expansion pass, exactly the move reserved for `action: [a,b,c]`:

1. Each entry on state `s` lowers to an implicit state named **`s#key`** (e.g.
   `generate#spec`). `#` cannot appear in a JS identifier, so implicit states
   can never collide with user consts and their outputs can never be
   destructured by other states — inaccessibility is structural, not policed.
   (Validation gains the rule: user state names must be valid identifiers.)
2. **Graph rewrite:** every transition `→ s` retargets to `→ s#key1`; the
   implicit states chain (`s#key1 → s#key2 → … → s`). Guards evaluate at their
   original position; the chain's own edges are unconditional. If `s` was
   `initial`, initial moves to the head of the chain. Loop-backs pass through
   the chain too — deliberate: on a revisit the _need_ may have changed (new
   review feedback), and memo makes unchanged pairs free.
3. **Implicit-state defaults:** `memo: true`, `temperature: 0`,
   `reasoning: "low"`, `maxTurns: 1`, `maxOutputTokens: entry.maxTokens`, output
   `{text: string}`, no events, no tools. The slice budget IS the existing
   output-cap enforcement — no new mechanism.
4. **forEach inheritance:** when the consumer declares `forEach`, each implicit
   state inherits `over`, `as`, and `concurrency` (not `onItemFailure` — v1 pins
   `fail`; see open questions). The consumer's per-item scope maps `key` to
   `s#key.items[index].text`, aligned by index — the same zip-by-index contract
   `write_files` already relies on.
5. **Failures are the consumer's failures:** the consumer's `catch:` edges are
   copied onto each implicit state, and semantic/transient retries follow the
   consumer's policy. Authoring model: "my distillation failing is me failing."
6. **Accounting:** implicit transitions do NOT count toward
   `limits.maxTransitions` (loop bounds stay properties of the _authored_
   topology); tokens and cost count normally — `steps inspect` shows `s#key`
   indented under `s`.

`steps validate --print` shows the lowered machine; the sugar is fully
inspectable. The mock provider keys scripts by state name as always —
`generate#spec:` entries make CI deterministic with zero new mock surface.

## The distiller prompt (engine-owned)

One fixed system prompt, not user-templatable in v1:

> From SOURCE, extract only what is relevant to NEED. Prefer verbatim quotes.
> Preserve identifiers, signatures, paths, numbers, and error text exactly. No
> commentary, no restructuring, no invention. If nothing is relevant, say only:
> `(nothing relevant)`.

The user message is `NEED: <rendered for> \n SOURCE: <rendered from>`. This is
an _extractor_, not a summarizer — verbatim-quote bias is what makes a 0.6B–8B
local model trustworthy at the job. The `for:` string is the only prompt-shaping
knob; if that proves insufficient the escape hatch already exists (write a real
state), which is the correct pressure.

## Defaults (the local-first path)

- Model resolution: `entry.model` → `models.distiller` alias → machine default
  model. `steps validate` emits a note when distill is used with no `distiller`
  alias — the recommended shape is a cheap local ref (`lmstudio/…`, `ollama/…`).
  When model ladders land, `distiller` is the alias most obviously served by
  `["lmstudio/…", "openrouter/…"]`.
- `memo: true` by default: distillation is pure (no side effects, no tools), so
  replay is always safe — the same argument that forbids memo on actions permits
  it here unconditionally.
- Memo cascade: the consumer's memo hash covers its rendered prompt, which
  contains the distilled text — so a stable distillation (memo hit) feeds a
  stable consumer hash. Unchanged (file, feedback) pairs stay free end to end
  across build loops.

## Companion knob: `maxInputTokens`

The mirror of `maxOutputTokens`, and what makes the thesis _enforced_ rather
than aspirational:

As shipped (default-on):

- `maxInputTokens` cascades state → `defaults.maxInputTokens` → engine default
  **8192**. An author's `0` means off, per state or machine-wide. 8192 pairs
  with the 2048 output default inside the 16k window that is the practical
  local-model floor, and covers every shipped example with ~10× headroom.
- Rendered input (system + prompt, chars/4 estimate) over budget →
  `budget_exceeded` (exhaustion class: never retried, routable by `catch:`),
  **attributed**: the error names the largest destructured inputs
  (`largest inputs: spec ~6100, plan ~2100`) so the fix — `distill:` or trim —
  is one look away. Attribution is computed only on the overflow path.
- The implicit distiller states are exempt from every rung of the cascade — they
  are the one place the big payload is _supposed_ to appear. (A source exceeding
  the distiller model's actual context window still fails at the provider;
  map-reduce chunked distillation remains v1.x.)
- Still open: a `steps validate` warning when schema stubs make overflow
  statically plausible.

## Validation (fail before you spend)

- `from` (or the shadowed key) must resolve to a declared run input or a
  graph-predecessor state's output — the same reachability check as
  `history.from`. Engine-supplied keys (`visits`, `run`, `output`, …) are not
  distillable.
- `for:` functions dry-run against schema stubs like every other function.
- Shadowing rewrites the consumer's stub: the shadowed key becomes a string
  stub, so field access on it fails the load with "distilled to text by
  `distill.spec`" in the message.
- Distill entries are independent: every `for:` sees the _pre-distill_ scope (no
  entry may reference another's output; chain real states if you need staged
  compression).
- `maxTokens` must be < the consumer's `maxInputTokens` (when both set) — a
  slice that cannot fit its consumer is a load error, not a runtime one.

## What proves it (measurement, not vibes)

**Measured 2026-07-04** (live A/B on `examples/codegen`, gates on OpenRouter —
full numbers in the [example README](../examples/codegen/README.md)): the
mechanics all held — six coder visits re-distilled for zero tokens (memo), five
visits of `generate#build_cause` made no model calls (absent source), and the
one real build failure distilled to the exact root-cause line verbatim for 346
tokens. The economics did **not** pay on that fixture: its spec is already
slice-sized, so the distiller returned essentially the whole document at ~1.4k
tokens of overhead (~3% of the run). The honest guidance that falls out:
`distill:` is for sources **much larger than `maxTokens`** — a real spec, a
compiler transcript — not a reflex for every input. Run-to-run reviewer variance
(first-pass approve vs five rejections, same machine, temp 0) dominated totals
by 5×, so per-visit `steps inspect` numbers, not run totals, are the comparison
that means anything.

The original protocol, for re-running on a fixture big enough to show the slice
savings: run `examples/codegen` live through a multi-iteration build loop,
before and after the two distill entries, and compare `steps inspect` per-visit
input tokens to `generate` items and end-to-end cost. The claim: per-file input
cost drops from O(spec) to O(slice), and re-visits show `memo_hits` on both
`generate` AND `generate#spec`. If distilled runs don't converge as reliably
(slices lost something the coder needed), that is a finding about `for:`
authoring and goes in the example README either way.

## Open questions

1. **forEach `onItemFailure: skip` alignment** — if the consumer skips poisoned
   items, should a failed distillation skip the item too? v1 pins distill
   fan-outs to `fail` (routed by the consumer's `catch:`); revisit when a real
   machine hits it.
2. **Faithfulness checks** — a `check: (scope) => boolean` guard on the slice
   (e.g. "must contain the function signature") would catch lossy distillation
   at the boundary. Deferred: it's the tool-guard shape applied to distill, wait
   for evidence it's needed.
3. **Cross-entry staging** — distilling a distillation (map-reduce over huge
   sources). Deliberately excluded from v1; the flat-FSM answer is "write a real
   state," and that answer might just be correct.
4. ~~**Should `maxInputTokens` default tighter than 16k?**~~ Resolved
   (2026-07-04): defaulted on at **8192** — tighter than the sketch, per the
   local-model thesis — once overflow attribution existed to make the failure
   self-explanatory and `0` gave a one-line opt-out.

## Under the hood

_Design record — implementation status and the deltas from the original
sketch. Skip this unless you're changing how distill works._

**Status:** implemented (2026-07-04) — `machine/distill.go` is the lowering,
`engine.applyDistill` the scope mapping; acceptance coverage in
`machine/distill_test.go` and `engine/distill_test.go`. The companion knob
`maxInputTokens` is implemented as a **default-on** cap (8192, chars/4 estimate,
`0` opts out) with per-value attribution on overflow — it shipped opt-in first,
then defaulted on once the attribution existed to make the failure
self-explanatory.

> **Implementation deltas from this sketch:**
>
> - `maxInputTokens` shipped opt-in first, then **defaulted on at 8192** (half
>   the sketched 16k — the local-model thesis argues tighter): per-state or
>   `defaults.maxInputTokens`, chars/4 estimate over system + prompt,
>   over-budget classifies `budget_exceeded` and **names the largest
>   destructured inputs** (`largest inputs: spec ~6100, plan ~2100`).
>   `maxInputTokens: 0` opts a state or the machine out. Implicit distill states
>   are exempt from every rung of the cascade — the distiller is the one place
>   the big payload is supposed to appear.
> - `forEach.over` and flow guards see the **pre-distill** scope: `over` must
>   produce the same list for the implicit fan-out and the consumer (zip by
>   index), and guards run at the boundary, not inside the state.
> - The dry-run's shadowed-key stub permits string methods (`spec.trim()`) and
>   interpolation; any other field access fails the load naming the
>   distillation.
> - Distill hops don't count toward `limits.maxTransitions` and their
>   `transition_fired` events carry `implicit: true` (the fold skips them).
> - **Absent source = empty slice, for free.** A source state that has not
>   executed on this run's path (loop feedback before the loop — `build` stderr
>   on the coder's first visit) yields `""` with no model call, like
>   `adopt: "self"` on a first visit. The consumer's ternary
>   (`${build_cause ? ... : ""}`) reads it as falsy.
> - **Small source = verbatim pass-through, for free** (added after the
>   2026-07-04 measurement). If the rendered source fits the slice budget
>   (chars/4 estimate ≤ `maxTokens`), the identity is the best possible
>   extraction — the source crosses verbatim with no model call, `for:` is never
>   rendered, and the ledger records `passthrough` / `passthrough_hits` like it
>   records memo. Distill is never-lose: small sources cost nothing, big sources
>   pay for real compression. If a pass-through then trips a consumer's
>   `maxInputTokens`, that is correct pressure — raise the slice budget so
>   extraction actually runs.
> - User state names are now validated as JS identifiers, which is what keeps
>   the lowered `name#key` namespace collision-free.
