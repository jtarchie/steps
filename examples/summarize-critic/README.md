# summarize-critic

The canonical `steps` example and the **acceptance spec for v1**: every command in this
README must work when v1 ships. It is deliberately small (two agents, one action, one
human gate) while exercising nearly every feature in [DESIGN.md](../../DESIGN.md).

## What it exercises

| Feature | Where |
|---|---|
| Micro-agents with per-state models | `draft` (qwen3:8b) vs `critique` (llama3.2:3b) |
| Agent proposes, guards dispose | `critique` emits `approve`/`revise`; guard functions veto |
| Engine-bounded loops | `visits.draft < 3` guard + `maxTransitions: 12` backstop |
| Explicit contracts | typed output schemas; prompt functions are the input contract |
| Feedback loops between states | rejected draft destructures `({ critique })` on the next pass |
| Semantic retry (re-prompt with error) | mock's non-JSON critique response; small local models also trigger this naturally |
| Transient retry (backoff) | mock's `rate_limited` injection; or kill Ollama mid-run |
| Fallback transitions | `critique` falls through to `escalate` |
| Human gate + park/resume + timeout | `escalate` with `timeout: "1h"` + a `timeout:` route in the flow |
| Builtin tool library | `file.write` in `publish` |
| Defaults | no `initial`, no transitions on `draft`/`publish`, implicit `done`/`failed` |
| Durability | kill the process mid-run, `steps resume` finishes it |
| Multi-provider | change one `model:` line, nothing else moves |

## Paired variant: `adopt: self`

[`../summarize-critic-adopt/`](../summarize-critic-adopt/README.md) is the same machine
with one change: the drafter continues its own conversation across revisions
(`adopt: "self"`, context rung 3) instead of being re-primed with distilled feedback
(`ctx`, rung 1, this example). It shares this example's mock script and article
fixture, so running both A/B-tests the two context philosophies — token cost per
revision, transcript growth, anchoring behavior — with the context mechanics as the
only variable. Its README carries the delta table and the extra assertions.

## Prerequisites (live mode only)

```sh
ollama pull qwen3:8b
ollama pull llama3.2:3b
```

For LM Studio instead of Ollama, either change the `model:` refs to
`lmstudio/<model-id>` (see `lms ls`), or leave them and point the ollama
provider at LM Studio's server: `OLLAMA_BASE_URL=http://localhost:1234/v1`.

Mock mode needs nothing — no network, no models, no keys.

## Run it

```sh
# 1. Deterministic (CI): scripted provider, exact journal assertions
steps run workflow.ts --input article=@fixtures/article.txt --mock mock_responses.yaml

# 2. Live local iteration
steps run workflow.ts --input article=@fixtures/article.txt

# 3. Validate without running — dry-runs every function; typos fail here
steps validate workflow.ts --print

# 4. Human gate: force escalation (mock file where critique never approves),
#    then resume the parked run
steps runs list
steps resume <run-id> --event approved

# 5. Durability drill
steps run workflow.ts --input article=@fixtures/article.txt &
kill -9 %1
steps resume <run-id>

# 6. Swap providers — edit defaults.agent.model to anthropic/claude-haiku-4-5
```

## Expected mock trace (what CI asserts)

With `mock_responses.yaml`, the run is fully deterministic:

```
run_started
state_entered    draft (visit 1)
handler_finished draft            -> weak summary
transition_fired draft -> critique                    (linear default)
state_entered    critique
  attempt 1: rate_limited         -> transient retry, backoff journaled
  attempt 2: non-JSON output      -> semantic retry, feedback appended
  attempt 3: score 4, event=revise
transition_fired critique -> draft                    (on: revise, visits.draft < 3)
state_entered    draft (visit 2)  -> prompt now contains critique.issues
handler_finished draft            -> improved summary
transition_fired draft -> critique
state_entered    critique
  attempt 1: score 9, event=approve
transition_fired critique -> publish                  (on: approve, score >= 8)
state_entered    publish          -> writes out/summary.md
transition_fired publish -> done                      (linear default)
run_finished     done
```

Assertions: exact event sequence above; `visits.draft == 2`; one transient retry and
one semantic retry on `critique`; `out/summary.md` exists and contains the three key
points; total transitions = 5 (well under the limit of 12).

Live-mode assertions are invariants only (never content): the run reaches `done` or
parks at `escalate`; `visits.draft <= 3`; every output validated against its schema.
