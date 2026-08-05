# Agent internals

How agent steps behave underneath: transport-level repair, loop detection,
provider-specific caching, retry/timeout interaction, and context compaction.

None of this is configuration you write day to day — see [agents.md](agents.md)
for that. This page is here for when something behaves in a way the authoring
guide doesn't explain, or when you're changing this code.

## Repairing malformed tool-call arguments

An OpenAI-compatible server sends a tool call's `arguments` as a JSON *string*, and weak local models (LM Studio, Ollama class) malformed it often: truncated at `max_tokens` mid-string, trailing commas, prose or markdown fences around the object. The LLM adapter parses that string leniently — on *any* parse failure it silently substitutes an **empty map**, so the call arrived at the conversation as if the model had passed no arguments at all: the tool answered "missing required argument", the model had no idea why, and a coding agent could burn most of its turn budget rediscovering the failure.

Every agent's HTTP client therefore carries a **response-repair transport** (`internal/agent/repair.go`) that inspects each chat-completion response and, for any `arguments` string that doesn't parse, attempts a minimal best-effort repair before the adapter ever sees it — the same validate → repair → re-validate shape as crush/fantasy's tool-call repair, placed at the transport because that is the last place the model's raw text exists. The rule set covers only what local models actually produce: dropping prose/fences around the object, closing an unterminated string and unclosed braces (truncation), supplying `null` for a dangling key, and stripping trailing commas. Anything else is left exactly as-is — an unrepairable call behaves precisely as it did before this existed, so repair can only recover a call, never alter a valid one (an all-valid response passes through byte-identically, and non-chat requests are never touched).

Transport-level only, like the OpenRouter caching levers: no config surface, no merkle impact, and it applies to **every** provider, since malformed arguments are an OpenAI-compat-wide hazard rather than a provider-specific one.

## Loop detection

`max_turns` bounds how long a conversation can run, but it is a blunt backstop: an agent that re-issues the *identical* tool call and gets the *identical* result — re-reading a file it has already read, re-running a command whose output cannot change — is stuck, and before loop detection it would spin out its entire turn budget producing nothing. The conversation loop now hashes each tool **interaction** (tool name + arguments + result; `internal/agent/loop.go`, copied from crush's `loop_detection.go`) and counts repeats within a sliding window of the last 10 tool-executing turns.

The result is part of the hash so that *productive* repetition never trips the detector: re-reading a file after an edit returns different bytes, and a verify command's output changes as the tree is fixed. Only call-and-result-both-identical accumulates.

The reaction is two-strike. The first time one interaction exceeds 5 copies in the window, the conversation gets a warning message naming the tool and telling the model to change approach — the window is **not** reset, so the warning is the model's only chance. A second detection (i.e. it repeated the same interaction once more) fails the attempt as a task failure (`failed`, not `errored`, for hook dispatch) with "agent stuck in a loop". Unlike crush's window — which only starts counting after 10 full turns — any count over the threshold triggers, so an agent with the default 8 `max_turns` is protected exactly like one with 200. The detector is scoped to the conversation, which now runs exactly once per step.

## OpenRouter prompt caching

An agent whose `model:` resolves to OpenRouter (the `openrouter/` prefix, or a `source.endpoint:` pointing at `openrouter.ai`) gets two request mutations automatically. Neither is on by default in a stock OpenAI-compatible client, and without them a conversation pays full input price on every turn — exactly where caching is worth the most, since the whole prior history is re-sent each time.

- **`x-session-id`** — **one identifier per agent, per job run**, so OpenRouter pins that agent's calls to a single provider instance and its prompt cache stays warm. Without it, sticky routing only engages *after* a cache hit is observed, which is too late for a short job.
- **`cache_control: {type: ephemeral}`** — a top-level body field, OpenRouter's "automatic" caching form: it caches through the last cacheable block and advances the boundary as the conversation grows, rather than spending one of the four explicit per-message breakpoints. Injected **only** for Anthropic-routed models (`anthropic/…` or `~anthropic/…`), the family that needs an explicit marker. Providers with implicit caching (OpenAI, Gemini, DeepSeek, Groq, …) are left alone; sending them a marker risks a 400 for no gain.

### Why the session is scoped per agent

OpenRouter tracks sticky routing "at the account level, per model, and per conversation" — and an `agents:` entry *is* the (model, persona, dials) bundle, so two different agents share no cacheable prefix and are not one conversation. The session key is therefore the job run **plus the agent name**:

| | Same session? | Why |
|---|---|---|
| Turns of one step's conversation loop | yes | the whole prior history is re-sent each turn — the big win |
| A `to:`/`verdicts:` revise loop re-entering a step | yes | same agent, same prefix, across separate visits |
| A sub-agent called repeatedly by its parent | yes | same persona every call |
| A `fix:` agent | yes | same persona every call |
| Two different agents in one job | **no** | different model and persona — nothing to reuse |
| The same agent in two runs (incl. concurrent) | **no** | no cross-run provider pin |
| An `attempts:` request retry | yes | it continues the same conversation — see below |

A retry used to **break** the pin instead of extending it, on the reasoning that the failure being retried might be the pinned provider's own — and little was lost, because a retry restarted the conversation and a shared session could only ever have reused the short system+tools+prompt prefix anyway.

That reasoning inverted when `attempts:` became a request-level retry. A retry now *continues* the same conversation, so the accumulated prefix is exactly what it wants to reuse, and the session stays put. It is stable for the life of a `(run, agent)` with no retry component at all.

The per-agent split is not merely tidy. With a **router model** (`openrouter/auto` and friends) a session pins the *resolved model*, not just the provider — so under a run-wide session whichever agent ran first would silently choose the concrete model for every later agent in the job.

Keying on the agent **name** rather than the step index is equally deliberate: scoping per step *invocation* would fragment exactly the revise-loop and repeated-sub-agent cases where caching pays off most.

Both mutations are transport-level only:

- **No config surface.** There is nothing to opt into and nothing to set — pointing an agent at OpenRouter is the whole trigger.
- **No merkle impact.** The session ID and cache marker never enter a step's hashed content, so enabling caching cannot invalidate a cached step, and the same pipeline hashes identically before and after.
- **No effect on other providers.** A non-OpenRouter base URL gets no custom HTTP client at all, leaving `openai-go` to build its own exactly as it did before this existed.

Cached-token accounting is deliberately not surfaced: `steps` tracks no token usage anywhere, so there is nowhere to report a hit rate. Check the OpenRouter activity dashboard to confirm caching is landing.

## Timeout and Attempts on Agent Steps

Agent steps can set `timeout:` and `attempts:` to bound their execution:

```yaml
- agent: reviewer
  prompt: "Review the PR"
  timeout: 10m
  attempts: 2
```

**Timeout** bounds the **entire conversation** (all turns, all tool calls, and any request retries within them) to a wall-clock deadline. The built-in `agentStepTimeout` is 10 minutes; `timeout:` (if set) overrides it. A timeout mid-conversation classifies as **errored**, not failed.

**Attempts** retries the failing **request**, in `requests.go`'s transport. The conversation runs exactly once and carries on from where it was, so nothing accumulated is lost. Tool failures never trigger it at all — they come back to the model as data.

A task step's `fix:` agent can also set `attempts:` and `timeout:` independently:

```yaml
- task: test.sh
  attempts: 3
  fix:
    agent: fixer
    timeout: 5m
    attempts: 2
```

The fix agent's timeout/attempts are separate budgets — they don't consume or conflict with the task's retry count or timeout.

### Why `attempts:` lives in a transport

`internal/agent/requests.go` implements `attempts:` as `requestRetryTransport`, innermost in the client's stack. Two properties force it there.

**It is the only layer that can see a request.** A request is the operation `attempts:` names, and everything above the transport deals in conversations.

**It is the only layer that can switch off the client's own retry.** `openai-go/v3` retries every request twice by default, and `adk-utils-go`'s `genai/openai` `Config` exposes only `APIKey`, `BaseURL`, `ModelName`, and `HTTPOptions` — no `MaxRetries`. A retry loop anywhere *above* the transport therefore **multiplies** with that hidden one instead of replacing it, which is exactly the `attempts: × 3` problem that used to make `attempts: 6` cost up to eighteen requests. From inside the transport the retry can be refused, using the two mechanisms openai-go documents:

- `x-should-retry: false` set on the **response** — `requestconfig.shouldRetry` honors it above the status code. This covers every failure that reached a server.
- `X-Stainless-Retry-Count` read off the **request** — the client's own counter, `0` on a first send. A connection error has no response to carry a header, and the client retries those unconditionally, so the transport stashes the exhausted error and hands it straight back on any round above 0, without touching the network.

Both paths are covered by tests in `requests_test.go`, and `e2e_test.go` pins the request count end to end — a dependency bump that breaks either mechanism fails the suite rather than silently re-pricing every failing call.

One accounting detail: three failing requests produce two `agent.request_retry` lines. The third failure is the one the step reports, via `agent.conversation_failed` with the full `provider_requests` count beside it; logging it in both places would double-count the number this exists to make honest.

See [attempts-timeout.md](attempts-timeout.md) for a detailed guide covering the interaction with `assert:`, hook firing, and other step types.

## Compacting long conversations

Every model response and every tool result is normally appended to an agent step's conversation forever, for up to `max_turns` turns. A long-running agent — many tool calls, large file reads — can grow past the model's real context window well before hitting that turn cap; the provider call then errors out, and if `attempts:` allows a retry, the *whole* conversation restarts from scratch rather than degrading gracefully.

`compact_after_tokens:` bounds this, on an `agents:` entry:

```yaml
agents:
- name: coder
  source: { model: openrouter/anthropic/claude-3.5-sonnet }
  max_turns: 60
  compact_after_tokens: 40000    # smaller than the 102,400 default -- see below
```

Once a conversation's estimated size crosses the budget, the agent's own model is asked to summarize everything older than a recent window (roughly the most recent 30% of the budget), and the conversation continues from `[summary] + [recent turns]` instead of the full history. This can happen more than once in a very long conversation — each pass folds the previous summary into the new one. A summarization failure is logged and the turn proceeds uncompacted; it never aborts the attempt, the same failure-is-data treatment a tool failure gets.

**On by default, at 102,400 tokens (80% of an assumed 128K context window) — unlike every other feature on this page.** An agent that sets no `compact_after_tokens:` still gets compaction; `compact_after_tokens: 0` is what disables it. This is a deliberate exception to this codebase's usual value-gating contract for opt-in features ("absent, its behavior ... byte-identical to before it existed") — merkle hashes are unaffected either way (see below), but the conversation's *behavior* differs for any pipeline whose agent crosses the budget. The 20% headroom below a full 128K window is load-bearing, not padding: the size estimate covers the conversation alone, never the system prompt or the tool schemas resent with every request, so a budget set at the full window would only ever fire after a request had already overflowed.

**Small-context and local models must lower it.** 102,400 tokens is close to an entire typical context window — a 32K-token local model (LM Studio, Ollama) will overflow long before the default ever triggers. Set a budget comfortably under the model's real window, e.g. `compact_after_tokens: 20000` for a 32K model:

```yaml
- name: reviewer
  source: { model: lmstudio/qwen2.5-coder }
  max_turns: 60
  compact_after_tokens: 20000     # the 32K default would never fire in time
```

**The count is a local estimate, not accounting.** It's the same `len(text)/4` heuristic used elsewhere, applied to the conversation's own content — never the provider's real token-usage data. "`steps` tracks no token usage anywhere" (see the OpenRouter section above) still holds; this is a size heuristic that decides *when to compact*, not a usage figure anything reports.

### Narrowing one tool's inline budget

The 32,000-byte cap is global, which is the right default but has no answer for a tool whose output is mostly noise by construction — a fuzzy search returning a ranked list where the answer is the first few entries and the tail costs context on every subsequent turn. `max_output_bytes:` on a grant lowers the budget for that one tool:

```yaml
tools:
- mcp: gopls
  tools: [go_symbol_references]
  max_output_bytes: 6000
```

It can only **narrow**, never widen: a value at or above the global cap resolves back to the global cap. Narrowing loses no data either — overflow still spills to a file the model can read back — it only shrinks what lands inline.

It is rejected on **built-ins**, which already carry their own output contract (`read_file` pages, `list_dir` counts entries, `search_files` is bounded by arithmetic); stacking a second, conflicting bound on a designed one is a bug surface rather than a knob. It is also rejected on **sub-agent tools**, whose result is another agent's considered answer rather than a data dump. It is valid on custom tools and on all three MCP grant forms — unlike `description`/`required`/`max_calls`, which stay single-`tool:`-only, because the tool worth capping is typically one noisy member of a `tools: [...]` subset and making it carry the cap in its own grant entry would open a second connection to the same server.

`max_output_bytes:` **is** part of the merkle hash (value-gated, so leaving it unset hashes byte-identically to before the field existed) — it changes what a call returns to the model, and therefore the conversation the step produces.

**Compaction is lossy.** Tool results are truncated to 4000 bytes apiece in the summarization prompt, and everything older than the retained window survives only as prose in the summary — not verbatim. Separately, and independently of compaction: any tool result too large to return inline (over 32,000 bytes — `run_shell`/a custom tool, an MCP tool's text or structured content, a sub-agent's final answer, a `previous_run` field or trajectory arg, a `fix:` loop's failure output) is instead saved to a file under the step's working directory, with a short `<persistent_file>` pointer message (path, size, a preview) taking its place in the conversation. That pointer message is well under 4000 bytes, so it survives compaction's own truncation intact — a compacted agent can still `read_file` whatever it had already gathered before compaction, even though the raw conversation turn that produced it is gone. `read_file` itself is the exception to the spill-to-file treatment: it reads up to 100,000 bytes in a single call — deliberately larger than the 32,000-byte spill threshold, so a spilled file can always be pulled back whole in one read rather than looping the model on a truncated prefix — and an oversized *file* read degrades to a plain truncation (spilling a file read back out to another file would be a pointless loop) with `start_line`/`end_line` to page through the rest.

**Not part of the merkle hash.** Like `timeout:`/`attempts:`, `compact_after_tokens:` is operational — it changes how a conversation manages its own context budget, not the result it's aiming for — so it never enters a step's hashed content and cannot invalidate a cached step, in either direction.
