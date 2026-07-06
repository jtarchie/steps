# Writing terser workflows — the DSL sugar

A machine is state consts plus one `flow:` expression. That works, but a few
shapes come up again and again — a judge/revise loop with a human tie-break, a
prompt that re-injects upstream values, per-role model settings. This guide
covers six pieces of sugar that make those shapes shorter to write, without
changing what the machine does.

Everything here is optional and everything **compiles down to the plain
machine** you'd otherwise write by hand. Run
`steps validate --print
your-workflow.ts` any time to see the expanded version —
the sugar disappears and you get the real states and transitions. So you can
adopt one piece, mix sugar with hand-written flow, or ignore all of it.

**Learn by diffing.** Three examples ship in two forms — the original
`workflow.ts` and a `workflow-dsl.ts` twin that uses this sugar. They're the
same machine and run identically. Open a pair side by side:

- `examples/summarize-critic/` — `verdict:`, `evidence:`, loop `escalate:`
- `examples/codegen/` — all six, on a two-gate build pipeline
- `examples/pr-review/` — model tiers

Quick reference:

| You want to…                         | Use                           |
| ------------------------------------ | ----------------------------- |
| Say when a judge accepts, once       | [`verdict:`](#verdict)        |
| Ask a human when a loop gives up     | [loop `escalate:`](#escalate) |
| Add a human decision point anywhere  | [`gate()`](#gate)             |
| Stop hand-plumbing prompt values     | [`evidence:`](#evidence)      |
| Name model roles once (scout/senior) | [model tiers](#tiers)         |
| Fan out and keep results aligned     | [`forEach carry`](#carry)     |

---

<a id="verdict"></a>

## `verdict:` — say when a judge accepts, once

A judging state usually restates its pass/fail rule three times: a `score` in
the output schema, an `events: ["approve", "revise"]` list, and a separate
`accept:` guard on the loop. Declare it once instead, right on the judge:

```ts
const critique: State = {
  prompt: "Score the summary 0-10 …",
  output: { score: "number", issues: "string[]" },
  verdict: ({ output }) => output.score >= 8,   // ← the whole acceptance rule
};

// the loop no longer needs accept: — it uses the judge's verdict
flow: pipe(loop(draft, { judge: critique, maxVisits: 3, … }), publish),
```

`verdict:` is a function of the state's result (`{ output, event }`) returning a
boolean. `loop()` picks it up automatically when you omit `accept:`. You can
drop the `events:` line too — the loop routes by the verdict, not by an event
the model has to emit.

**Gotchas**

- Passing both `verdict:` on the judge and `accept:` on the loop is an error —
  pick one.
- Only states that produce output (agents, actions) can have a `verdict:`.

---

<a id="escalate"></a>

## Loop `escalate:` — ask a human when the loop gives up

The most common thing to do when a revise loop runs out of tries is: ask a
person whether to ship the current draft or fail. Written by hand that's a whole
`human` state plus a three-way branch. As a loop option it's two lines:

```ts
loop(draft, {
  judge: critique,
  maxVisits: 3,
  escalate: {
    prompt: ({ critique }) =>
      `Out of revisions (last score ${critique.score}). Approve or fail?`,
    timeout: "1h",
  },
});
```

`escalate:` builds a human gate for you. **Approve** continues to wherever the
loop's success path goes; **reject** or **timeout** fail the run. You can pass
just a prompt (`escalate: "Approve or fail?"`) or `{ prompt, timeout }`.

**Gotchas**

- `escalate:` and `exhausted:` do the same job two ways — pass one, not both.
  Use `exhausted:` when you want to route somewhere other than "approve ships /
  reject fails".

---

<a id="gate"></a>

## `gate()` — a human decision point anywhere

`escalate:` is the loop-shaped shorthand; `gate()` is the general version you
can drop into any flow position (a branch edge, mid-pipe, a loop's
`exhausted:`).

```ts
// simple: approve continues, reject/timeout fail
gate("ship_it", {
  prompt: "The change looks risky. Ship anyway?",
  approve: deploy,
  timeout: "1h",
});

// full control: name every option and where it goes
gate("review", {
  prompt: "How should we handle this PR?",
  choices: {
    merge: { to: do_merge, label: "Merge as-is" },
    comment: { to: post_notes, label: "Request changes" },
    close: fail,
  },
});
```

Two forms: **`approve:`** (approve → your target, reject/timeout → fail) for the
common yes/no, or **`choices:`** for a full menu. Targets can be states or
nested flow (`pipe(...)`, another `gate(...)`).

The human state it creates is named `gate#<name>` and shows up in
`steps validate --print` and the run diagram like any other gate.

**Gotchas**

- Already have a state literally named `gate`? That still works — naming a
  `const gate = { human: … }` shadows the combinator. Only _calling_ `gate(...)`
  uses the sugar.

---

<a id="evidence"></a>

## `evidence:` — stop hand-plumbing prompt values

Most prompts are an instruction plus some upstream values pasted in under
headers, often with an "if this exists, include it" conditional. That plumbing
is noisy inside a template string:

```ts
// before — instruction and plumbing tangled together
prompt: ({ article, critique }) => `
  Summarize the article in 150 words, then give three key points.
  ${critique ? "A reviewer rejected the draft:\n" + list(critique.issues) + "\nFix every issue." : ""}
  ARTICLE:
  ${article}`,
```

Split them: `prompt:` is just the instruction, `evidence:` lists the blocks to
append.

```ts
// after — instruction is plain English; the plumbing is data
prompt: "Summarize the article in 150 words, then give three key points.",
evidence: {
  article: true,                                    // paste in the `article` value
  reviewer_feedback: ({ critique }) =>              // computed; skipped if empty
    critique && `A reviewer rejected the draft:\n${list(critique.issues)}\nFix every issue.`,
},
```

Each entry becomes a labeled block appended to the prompt, in order:

```
Summarize the article in 150 words, then give three key points.

ARTICLE:
<the article text>

REVIEWER FEEDBACK:
<the issues, only when there was a critique>
```

Entry forms:

- **`key: true`** — paste in the scope value called `key` (a run input, an
  upstream state's output, a distilled key, or the forEach item).
- **a function** — compute the block. **If it returns something empty**
  (`undefined`, `null`, `false`, `""`), the block is left out — that's how the
  reviewer-feedback block disappears on the first draft, no ternary needed.
- **a string** — a constant block.

The header comes from the key: `reviewer_feedback` → `REVIEWER FEEDBACK:`.
Objects and arrays render as YAML.

**Gotchas**

- `evidence:` is for _plumbing_, not composition. A block that genuinely builds
  text (`generate.items.map(...).join(...)`) stays a function — it just gets a
  label. The big wins are on prompts that are mostly "paste this in,
  conditionally paste that in".
- Everything is still checked at load: a typo in a block function, or an
  `x: true` naming something that isn't in scope, fails `steps validate` with
  the available names — before any tokens are spent.

---

<a id="tiers"></a>

## Model tiers — name a role once, not per state

If several states share "cheap, low-reasoning" or "big, careful" settings, put
those settings on the `models:` alias instead of repeating them on every state:

```ts
models: {
  scout:  { model: "lmstudio/…gemma-3-4b",  reasoning: "low",  memo: true },
  senior: { model: "lmstudio/…gemma-3-27b", reasoning: "high", maxOutputTokens: 8192 },
  distiller: "openrouter/qwen/qwen3-coder-30b-a3b-instruct",   // a plain string still works
}
```

Then states just pick a role:

```ts
const triage: State = { model: "scout", prompt: "…" }; // gets reasoning low, memo on
const analyze: State = { model: "senior", prompt: "…" }; // gets reasoning high, 8192 tokens
```

A tier bundles `model` (required) plus any of `reasoning`, `maxOutputTokens`,
`memo`. A state can still override any of them inline — **the state always
wins** over the tier, the tier wins over machine `defaults:`.

**Gotchas**

- If a state picks its model with a _function_
  (`model: ({ risk }) => risk === "high" ? "senior" : "scout"`), the tier only
  supplies the model ref — the knobs (`reasoning`, etc.) are fixed at load, so
  keep those on the state. (See `pr-review`'s `deep_review`.)

---

<a id="carry"></a>

## `forEach carry` — fan out and keep results aligned

When you fan a state out over a list, the results come back as
`{ items: [...], count }`. If a later state needs to line each result up with
the input it came from, people zip by index (`plan.files[i]` against
`generate.items[i]`) — which quietly breaks the moment `onItemFailure: "skip"`
drops a failed item and shifts everything.

`carry: true` pairs them for you:

```ts
forEach: { over: ({ plan }) => plan.files, as: "target", carry: true },
```

Now each entry is `{ item, output, index }` — the source item, its result, and
its position in the original list:

```ts
// before (fragile): zip two lists by index
over: ({ plan, generate }) => plan.files.map((f, i) => ({ path: f.path, content: generate.items[i].text })),

// after (safe): each result already carries its source
over: ({ generate }) => generate.items.map((e) => ({ path: e.item.path, content: e.output.text })),
```

`index` is the position in the _original_ list, so the pairing stays correct
even after skipped items.

**Gotchas**

- It's opt-in, so nothing changes for foreach states that don't set it.
- With `carry`, downstream code reads `items[i].output` / `items[i].item` — a
  bare `items[i].field` is caught at load and told to use the new shape.

---

## Where to go next

- `steps validate --print your-workflow.ts` — see any of this expanded to the
  plain machine.
- `steps context your-workflow.ts` — see what each state's functions may
  reference (inputs, upstream outputs, distilled keys).
- The `workflow-dsl.ts` example twins — the same machines written with this
  sugar, verified to run identically to their originals.
- `docs/src/global.d.ts` — editor autocomplete and types for every option here.
