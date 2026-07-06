# codegen

A spec goes in; working, **checked** code comes out. This example is the `steps`
thesis at its sharpest — _determinism at the boundaries, choice in the interior_
— with **two gates** wrapped around a stochastic middle:

1. an architect turns prose into a typed file plan (once),
2. a coder **fans out** over the plan — one hermetic context per file,
3. **gate one, the reader:** an LLM reviewer scores the tree and, on approve,
   lets it reach disk (rung-1 `ctx`, fallible judgement),
4. **gate two, the ground truth:** a _real_ build/test command runs against the
   written files. Its exit code is the verdict — an LLM reviewer can be fooled,
   a compiler cannot.

Either gate can send the coder back with feedback: the reader loop fixes what a
human reader would catch (`review.issues`), the build loop fixes what only the
toolchain knows (`build_cause` — the build record distilled to its root cause).
`visits` bounds both loops; a human breaks the tie when a budget is spent.

The machine is **language-agnostic**: `language` and `verify_cmd` are run
inputs. Point it at Go with `verify_cmd="go build ./... && go test ./..."`, at
Python with `pytest -q`, at anything with an exit code.

## The second gate is the point

Most "agentic codegen" stops at gate one — a model reviewing a model. This
machine adds `exec.run`, a builtin action that runs a shell command as a
**gate**:

> A non-zero exit is **data**, not an exception. `exec.run` returns
> `{ok, exit_code, stdout, stderr}`; only a genuine failure to _launch_ (no
> shell, bad cwd) or a timeout raises. That distinction is load-bearing: if a
> failed build raised, the engine would classify it a transient `action_error`
> and replay the **same broken code** three times before failing the run.
> Returning the verdict as data lets a guard route on it —
> `build red → loop back to the coder with the distilled root cause → fix`.

`verify_cmd` is a rendered `input:` block — operator-authored, never model text
— so `exec.run` is safe as an _action_. Do **not** hand it to a model as a
`tool`.

We watched this pay off live: the reviewer approved a tree with a perfect
**10/10**, and then the real `bash greet_test.sh` **failed (exit 1)**. The build
loop kicked it back to the coder, the second attempt passed, and the run
finished green. The stochastic interior was fooled; the deterministic boundary
was not.

## What it exercises

| Feature                               | Where                                                                                                                                                                              |
| ------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Plan → fan-out                        | `plan` emits a file list; `generate` `forEach`-maps over it                                                                                                                        |
| Hermetic per-item context             | each file is its own conversation; no file sees its siblings                                                                                                                       |
| Per-state models                      | `architect` plans, `coder` writes, `reviewer` judges — three aliases, swap freely                                                                                                  |
| Agent proposes, guards dispose        | `review` emits `approve`/`revise`; the score guard vetoes                                                                                                                          |
| Two feedback loops into one state     | `generate` destructures `({ review, build_cause })` — reader issues _and_ the distilled build failure                                                                              |
| Declared context slicing (`distill:`) | `generate#spec` slices the spec per file; `generate#build_cause` boils the build record down to its root cause — see [docs/distill.md](../../docs/distill.md)                      |
| Engine-bounded loops                  | two nested `loop()`s — reader: `maxVisits: 5` on judge `review`; build: `maxVisits: 4` on judge `build` — each loop owns its budget + `maxTransitions: 40` (distill hops are free) |
| An **action** as a loop judge         | the inner `loop()`'s judge is `build`; `accept: ({ output }) => output.ok` routes on the exit code                                                                                 |
| `forEach` over an **action**          | `write_files` maps `file.write` — each write its own journal entry                                                                                                                 |
| Real build/test gate                  | `build` runs `exec.run`; guards route on `output.ok`                                                                                                                               |
| `memo`                                | `generate` caches per (file, feedback) — a build fix re-pays only touched files                                                                                                    |
| Human tie-break                       | `escalate` (reader spent / model choked) and `accept_build` (build spent)                                                                                                          |
| Function `write:` target              | `report` writes `${out}/GENERATED.md`                                                                                                                                              |

## Models & providers — a hard-won lesson

The gates (`plan`, `review`) need a model that reasons _and_ returns clean
structured JSON; the `coder` just writes files. They're `models:` aliases, so
you swap them in one place. As committed:

```ts
architect: "openrouter/qwen/qwen3-235b-a22b-2507",   // gate: the plan
coder:     "openrouter/qwen/qwen3-coder-30b-a3b-instruct",
reviewer:  "openrouter/qwen/qwen3-235b-a22b-2507",   // gate: the reader
distiller: "openrouter/qwen/qwen3-coder-30b-a3b-instruct", // context slicing; a small local model also fits
```

**Why the gates run on OpenRouter, not a raw local server.** These are reasoning
models, and the gate states set `reasoning: "low"` to keep the thinking short.
That maps to the standard `reasoning_effort` request field — which **OpenRouter
honors and LM Studio ignores** (LM Studio bug #988). When the knob is ignored, a
reasoning model spends its _entire_ output budget thinking and never emits the
answer (`budget_exceeded`); or, depending on LM Studio's "Separate
reasoning_content and content" setting, it files the answer into a
`reasoning_content` channel the engine treats as scratch and discards
(`model
produced no text`). Same model, same machine, behind a provider that
honors the field: it just works, no `/no_think` or `structuredOutput: "native"`
hacks.

This is **provider non-conformance**, and the fix belongs at the provider, not
in the machine or the engine. The engine deliberately does _not_ try to salvage
answers from the reasoning channel or retry a starved turn — that would be
permanent complexity to paper over one server's quirks, and could mask a
genuinely-too-small budget. Two honest ways to run it:

- **A conformant provider** (as committed): `openrouter/`, or any backend that
  honors `reasoning_effort` and returns the answer in `content`.
- **Fully local**: point the gate aliases at `lmstudio/…` (or `ollama/…`) but
  **turn thinking off for the gate model at the server** — LM Studio's model
  thinking toggle, or `enable_thinking=false` in its chat template — since the
  API-level `reasoning:` knob won't reach it. (The `coder` isn't a reasoning
  model, so it's fine local either way.) The
  `catch: { budget_exceeded: escalate }` on `review` is the last-resort net if a
  local reasoning model still runs away.

The `openrouter/` provider is first-class (`provider/openrouter.go`): it adds
`x-session-id` sticky routing (keyed on the run id, keeps the prompt cache
warm), `cache_control` for `openrouter/anthropic/*` models, and recovers
cached-token counts into usage. Set `OPENROUTER_API_KEY` (and optionally
`OPENROUTER_BASE_URL`).

## Run it

```sh
# 1. Deterministic (CI): the LLM states are scripted; the build gate runs for
#    real. Needs nothing but a POSIX `sh` — no network, no models, no keys.
steps run workflow.ts --mock mock_responses.yaml \
  --input spec=@fixtures/spec.md --input language=bash \
  --input out=out --input 'verify_cmd=bash greet_test.sh'

# 2. Validate without running — dry-runs every function; typos fail here.
steps validate workflow.ts --print

# 3. Live (as committed): gates on OpenRouter, real build gate.
export OPENROUTER_API_KEY=sk-or-...
steps run workflow.ts -v \
  --input spec=@fixtures/spec.md --input language=bash \
  --input out=out --input 'verify_cmd=bash greet_test.sh'

# 4. Live, a different target — language and gate are inputs, nothing else moves.
steps run workflow.ts \
  --input spec=@fixtures/my-spec.md --input language=go \
  --input out=./scratch --input 'verify_cmd=go build ./... && go test ./...'
```

The generated files land in `out/`, and `out/GENERATED.md` records what was
built and whether the gate went green.

## A real end-to-end run (live)

```
plan → generate → review (approve, score 10) → write_files
     → build  (bash greet_test.sh → exit 1, FAILED)           ← gate two vetoes the 10/10
     → generate (visit 2, fixes it) → review (approve, 10) → write_files
     → build  (exit 0, "All tests passed!") → report → done   (10 transitions)
```

The generated `greet.sh` handles `--name`/`--shout`, composes them, and exits
non-zero with usage on an unknown flag; `greet_test.sh` asserts all of it and
passes.

## Measured: distill, live (2026-07-04)

A/B on this fixture — the committed machine vs the same machine without
`distill:` — gates on OpenRouter, per-state numbers from `steps inspect`:

- **Every mechanism behaved.** `generate#spec` paid 630 in / 404 out once (visit
  1); all five reader-loop revisits replayed both slices from memo — ⚡ zero
  tokens, ten times. `generate#build_cause` made **no model calls for five
  visits** (no build yet — absent source), then, after the first real build
  failure, distilled the yaml build record to the exact root-cause line
  (`greet_test.sh: line 23: exit_code: unbound variable`, verbatim, both items)
  for 314 in / 32 out. The coder's input stayed flat at ~1k/visit across six
  visits.
- **On a fixture this small, slicing itself doesn't pay.** The whole spec is
  already slice-sized, so the distiller returned essentially the full document
  and the coder's visit-1 input was unchanged (797 vs 730 baseline). Total
  distill overhead: ~1.4k tokens, ~3% of the run. `distill:` buys tokens when
  **source ≫ maxTokens** — a real spec, a real compiler dump — and this fixture
  has neither. The parts that are free regardless (memo replay, the
  absent-source rule) are what showed up here. _This finding is now enforced by
  the runtime:_ a source that fits the slice budget passes through verbatim with
  no model call, so the overhead measured above is structurally zero on re-runs.
- **Run-to-run variance dwarfs the feature.** The baseline run's reviewer
  approved first pass (9.1k tokens total); the distill run's reviewer rejected
  five times on test-harness nitpicks (44.2k total — 65% of it the 27b
  reviewer's own thinking). Same machine, temperature 0, different generated
  code each round. Totals compare _runs_; the per-visit numbers above are the
  honest comparison.
- **The gates disagreed in both directions.** The reviewer nitpicked five rounds
  yet missed the actual bug the build caught (`exit_code: unbound
  variable`);
  the human override then approved past the one issue the reviewer had been
  right about (`./greet.sh` isn't executable — exit 126 on the second build).
  And because the reader loop had already spent `visits.generate`, the build
  loop got only one shot before parking at `accept_build` — the two loops shared
  one budget. _Fixed since:_ each loop is bounded on the gate that observes it,
  and the `loop()` combinator now bakes that lesson in — `maxVisits` always
  counts the judge's visits.

## Expected mock trace (what CI asserts)

The mock scripts a rejected first draft (`--shout` unimplemented), so the reader
loop fires once. Every coder visit enters through its distill chain first. The
build gate is **not** scripted — it genuinely runs `bash greet_test.sh` against
the written files:

```
run_started
state_entered    plan                 -> two files + contract + acceptance
transition_fired plan -> generate#spec                   (linear default, retargeted)
state_entered    generate#spec        -> pass-through x2: spec fits the budget, NO model calls
transition_fired generate#spec -> generate#build_cause   (implicit — free)
state_entered    generate#build_cause -> no build yet: "" x2, NO model calls
transition_fired generate#build_cause -> generate        (implicit — free)
state_entered    generate (visit 1)   -> greet.sh, greet_test.sh (foreach x2)
transition_fired generate -> review
state_entered    review               -> score 5, event=revise
transition_fired review -> generate#spec                 (revise: visits.review < 5)
state_entered    generate#spec        -> pass-through x2 again, still free
transition_fired generate#spec -> generate#build_cause   (implicit — free)
state_entered    generate#build_cause -> still no build: "" x2
transition_fired generate#build_cause -> generate        (implicit — free)
state_entered    generate (visit 2)   -> prompt now carries review.issues
transition_fired generate -> review
state_entered    review               -> score 9, event=approve
transition_fired review -> write_files                   (accept: score >= 8)
state_entered    write_files          -> writes out/greet.sh, out/greet_test.sh
transition_fired write_files -> build
state_entered    build                -> RUNS `bash greet_test.sh` -> exit 0, ok:true
transition_fired build -> report                         (when: output.ok)
state_entered    report               -> writes out/GENERATED.md
transition_fired report -> done
run_finished     done
```

Assertions (`engine/acceptance_test.go`): exact state sequence above; 12
journaled transitions but a **counted budget of 8** — the 4 distill hops are
implicit and free against `maxTransitions`;
`generate#spec.passthrough_hits == 2` (the fixture spec fits the slice budget,
so it crosses verbatim with zero model calls, every visit);
`generate#build_cause` yields two `""` slices with no model calls (absent
source); `generate.count == 2` (one hermetic context per planned file);
`build.ok == true` with the generated test's own `all tests passed` on stdout;
`out/greet.sh` contains the revised `--shout` handling; `out/GENERATED.md`
records `PASSED`.

On a **live** build failure the trace differs in one place: `build` red loops
back through the chain, `generate#build_cause` now has a source (the build
record, yaml-rendered) and distills the stderr dump down to `maxTokens: 200` of
root cause — so the coder's revisit context carries three lines, not the whole
compiler transcript, and `generate#spec` still replays from memo.

To watch **gate two** loop instead, hand it a failing command
(`verify_cmd='exit
1'`, with enough scripted rounds): `build` routes back to
`generate` with the distilled root cause until `visits.build` hits 4, then parks
at `accept_build` for a human. In live mode the loop closes on the real
toolchain — the coder keeps fixing until `verify_cmd` is green or the budget is
spent.
