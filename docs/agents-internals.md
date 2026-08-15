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

The reaction is two-strike. The first time one interaction exceeds 5 copies in the window, the conversation gets a warning message naming the tool and telling the model to change approach — the window is **not** reset, so the warning is the model's only chance. A second detection (i.e. it repeated the same interaction once more) fails the attempt as a task failure (`failed`, not `errored`, for hook dispatch) with "agent stuck in a loop". Unlike crush's window — which only starts counting after 10 full turns — any count over the threshold triggers, so an agent with the default 30 `max_turns` is protected exactly like one with 200. The detector is scoped to the conversation, which now runs exactly once per step.

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

Whether caching is landing is visible in `steps runs --cost`'s CACHED column — the provider's own reported cache figures, recorded per step (see [agents.md](agents.md#budgets-budgettokens)).

## Timeout and Attempts on Agent Steps

Agent steps can set `timeout:` and `attempts:` to bound their execution:

```yaml fragment
- agent: reviewer
  prompt: "Review the PR"
  timeout: 10m
  attempts: 2
```

**Timeout** bounds the **entire conversation** (all turns, all tool calls, and any request retries within them) to a wall-clock deadline. The built-in `agentStepTimeout` is 30 minutes; `timeout:` (if set) overrides it. A timeout mid-conversation classifies as **errored**, not failed.

**Attempts** retries the failing **request**, in `requests.go`'s transport. The conversation runs exactly once and carries on from where it was, so nothing accumulated is lost. Tool failures never trigger it at all — they come back to the model as data.

A task step's `fix:` agent can also set `attempts:` and `timeout:` independently:

```yaml fragment
- task: test
  run: ./test.sh
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

## CLI-backed agents: how delegation is wired

A `source.model` of `@claude/sonnet` replaces the conversation, not the endpoint. The authoring view is in [agents.md](agents.md); this is the mechanism.

### Where the branch lives

`config.resolveAgentTarget` dispatches on the leading `@` to `resolveCLITarget`, which populates `ResolvedInvocation.CLI` (the `cliProviders` key) and leaves `BaseURL` empty. Exactly one branch downstream reads it: `runPrepared` in `step.go`, which calls `runCLIConversation` instead of `runOneConversation`. Because `runPrepared` is the single choke point already shared by `RunStep`, `RunHook`, and every routed re-entry, all three get the CLI path from that one line.

`prepareAgentStep` is otherwise untouched: workspace, `buildAgentTools`, `injectSynthesizedTools`, context blocks, and the spill directory are all built identically. Only `newAgentLLM` is skipped — a CLI source has no HTTP client, so the OpenRouter/repair/retry transport stack has nothing to wrap.

Two tables describe a CLI, deliberately split by concern: `config.cliProviders` holds what a *load* needs (which `@name` values exist, which binary to look for), and `agent.cliRuntimes` holds what an *invocation* needs (the native tool mapping, the always-denied list). `TestCLIRuntimesCoverProviders` asserts they cover the same keys, so half-adding a CLI fails the build rather than a run.

### The bridge

`clibridge.go` starts an MCP server in the parent process, on a loopback listener with an ephemeral port, for the duration of one attempt. It re-exports every tool in the step's registry except the ones the CLI serves natively, and writes a `--mcp-config` document naming its URL. The config file goes to the OS temp dir, never the workspace — the workspace is captured as artifacts and readable by the agent's own file tools, and a live callback URL belongs in neither.

The server is **stateless** (`StreamableHTTPOptions{Stateless: true}`). It serves exactly one child for one attempt, so per-session state would only be a way for a crashed child to strand something.

Adaptation is thin because the tool contract already fits: a `toolImpl` never returns a Go error, so the only translation is which result shape becomes `IsError` — and `requiredCallSucceeded`, the same predicate the HTTP loop uses, already answers that.

The bridge is also where verdicts are captured. Every successful call is inspected for the `verdict`/`note` keys, in the parent's memory, at the moment the tool runs. Nothing depends on the CLI reporting what it decided.

### Enforcing required tools without `tool_choice`

The HTTP loop enforces `required: true` by forcing an unsatisfied tool through the next turn's `tool_choice`. That lever does not cross a process boundary. `checkCLIObligations` moves the check to the exit instead: `unsatisfiedRequiredTools` — again the same function — is consulted once the process is gone, and anything still unsatisfied is an `outcome.Fail`. That is strictly stronger than the hosted path's "force one more turn and hope", at the cost of not being able to nudge mid-conversation.

This is why tool guards (`required:`/`max_calls:`/`args:`) are rejected on CLI agents at load: only the synthesized tools have an enforcement point that survives the boundary.

### Two flags, two questions

The child's tool surface is set by `--tools` and its permissions by `--allowedTools`, and conflating them is the mistake worth avoiding.

`--tools` is the surface: the CLI offers only the built-ins named there, and nothing else exists for that step (verified — `--tools Read` reports exactly `["Read"]`). This is what makes the grant deny-by-default. The first implementation instead enumerated every known tool and denied the ungranted ones, which fails open the moment the CLI ships a tool this build has never heard of; `--tools` inverts that, so an unknown tool is excluded by never having been named.

`--allowedTools` is permission, a different axis. `Read`/`Glob`/`Grep` need none, but `Bash`, `Write` and `Edit` are gated and would otherwise stall on a prompt nobody can answer in `-p` mode — so a *granted* gated tool must be named here or the step silently loses a capability its pipeline asked for. It is also how the bridge's `mcp__steps__*` tools are permitted, since `--tools` governs built-ins only. Both halves are pinned live (`TestLiveCLIGrantedWriteActuallyWrites`, `TestLiveCLIUngrantedToolIsWithheld`).

### Configuration scope, and what is still not hashed

`--setting-sources` is passed empty by default: the subprocess loads **no configuration scopes** — not the operator's `~/.claude` (settings, hooks, plugins, skills, output styles — none of which appears in the merkle hash, so the same pipeline would behave differently per machine and a user hook could fire inside a step that never declared one), and not the repo's `.claude/` either. Repo config shaping an agent is a capability, so the pipeline opts into it: `settings: project` on the agent entry passes `--setting-sources project`.

When project scope is opted in, its *contents* are still **not hashed** — but the opt-in itself is (`cli_settings`), so granting or revoking the scope invalidates the cache. Folding the contents in would mean reading files that may sit outside the step's declared inputs, and the honest boundary is the same one this package already draws around the CLI's version: steps hashes *which* thing was asked for, not the state of everything on the other side of the call. A repo's `CLAUDE.md` changing does not invalidate a cached CLI agent step; if that matters for a given pipeline, name the file in `context_paths:`, which is hashed.

### Reading the transcript back

`clistream.go` parses the CLI's line-delimited JSON off a pipe as it arrives, rather than buffering it whole — so a step killed by its timeout still records the trajectory of what it managed to do. Parsing is tolerant by design: the event schema belongs to the CLI, so an unknown event type, an unexpected content block, or an unparsable line is logged at debug and skipped. The one intolerable case is a missing terminal `result` event, which distinguishes "the CLI died mid-sentence" from "the CLI finished with nothing to say" — and that distinction is what `attempts:` keys off.

Tool names enter the trajectory exactly as the CLI reported them (`Bash`, `Read`, `mcp__steps__verdict`), because that is what actually ran. Translating them back to steps' built-in names would make the record a reconstruction rather than an observation. Worth knowing when writing `assert.tool_calls` against a CLI step.

### Session continuity across retries

Each CLI step mints a v4 UUID and passes `--session-id` on its first invocation, then `--resume <same id>` on every retry. Minting rather than parsing the CLI's reported `session_id` buys two things: the resume needs no round trip through the transcript, and cleanup can match on a name no other run could own.

The load-bearing fact, verified rather than assumed: **session-scoped flags re-apply on resume.** A resumed invocation re-reads `--mcp-config`, which is what makes this compatible with the per-attempt bridge — each attempt binds a fresh ephemeral port, and a resume that inherited the first attempt's config would be talking to a dead socket. `--tools` re-applies the same way. Resume also survives `SIGKILL` mid-run, which matters because that is the actual retry trigger; `TestLiveCLIRetryResumesRealSession` drives exactly that path against the real binary.

`--max-turns` counts per invocation, not per session, so the cumulative budget is steps' to track: each attempt is passed `max_turns - turnsSpent`, and the step fails outright rather than retrying when nothing is left.

Session files are deleted afterwards by globbing `~/.claude/projects/*/<session>.jsonl`. Matching on the minted id rather than a derived directory name keeps the deletion precise — the only file that can match is one this step created — and the whole thing is best effort, because a step that did its work must never fail over tidying.

### Hashing

`AgentContentMap` folds in a `cli` key, value-gated so every pre-existing hosted agent hashes byte-identically and no cache entry was invalidated by this shipping. Moving an agent between a CLI and a hosted provider does change its hash, which is correct — it is a different thing producing the output.

The CLI's own **version is deliberately not hashed**. It drifts under the operator exactly the way a hosted model's weights do, and this package has never claimed to hash the thing on the other end of the wire, only which thing was asked for. Folding in `claude --version` would also put a process spawn in the planning path, which `steps plan` and `steps watch` run constantly.

### Preflight

A CLI target's probe is `exec.LookPath`, nothing more. The HTTP probe sends a real request because an endpoint can be reachable and still reject the model or the key; a CLI has no equivalent cheap failure, and spawning one to find out would put a process launch in every `steps watch` poll. A CLI that is installed but broken fails at the step with the CLI's own error, which beats anything a probe would synthesize.

The probe cache is keyed `cli|<cli>|<model>` rather than `model|<baseURL>|<model>`: `""` is a perfectly ordinary `BaseURL` for a CLI source, so a shared key space would let a CLI agent collide with an endpoint-less hosted one.

## Compacting long conversations

Every model response and every tool result is normally appended to an agent step's conversation forever, for up to `max_turns` turns. A long-running agent — many tool calls, large file reads — can grow past the model's real context window well before hitting that turn cap, and the provider call then errors out.

`compact_after_tokens:` bounds this, on an `agents:` entry:

```yaml test=internals-compaction
agents:
- name: coder
  source: { model: openrouter/qwen/qwen3.7-flash }
  max_turns: 60
  compact_after_tokens: 40000    # smaller than the 102,400 default -- see below

jobs:
- name: build
  plan:
  - agent: coder
    prompt: "Do the long-horizon work."
    assert:
      stdout: Done
  assert:
    execution: [coder]
    outcome: succeeded
```

Once a conversation's estimated size crosses the budget, the agent's own model is asked to summarize everything older than a recent window (roughly the most recent 30% of the budget), and the conversation continues from `[summary] + [recent turns]` instead of the full history. This can happen more than once in a very long conversation — each pass folds the previous summary into the new one. A summarization failure is logged and the turn proceeds uncompacted; it never aborts the attempt, the same failure-is-data treatment a tool failure gets.

**On by default, at 80% of the model's context window — unlike every other feature on this page.** An agent that sets no `compact_after_tokens:` still gets compaction; `compact_after_tokens: 0` is what disables it. This is a deliberate exception to this codebase's usual value-gating contract for opt-in features ("absent, its behavior ... byte-identical to before it existed") — merkle hashes are unaffected either way (see below), but the conversation's *behavior* differs for any pipeline whose agent crosses the budget.

The 20% headroom is load-bearing, not padding: the size estimate covers the conversation alone, never the system prompt or the tool schemas resent with every request, so a budget set at the full window would only ever fire after a request had already overflowed.

**The window comes from the model.** `internal/config/agent.go`'s `contextWindows` table maps a model-name fragment to that model's window, so a 1M-context model compacts at 800,000 rather than at a tenth of its capacity. A model the table does not recognize keeps the conservative 102,400 (80% of an assumed 128K) — the safe direction to be wrong in, and no behavior change for anything that was already correct.

Matching is on a *normalized* name — lowercased, with `.` folded to `-` — because the same model arrives spelled `claude-sonnet-4-5` from Anthropic and opencode but `claude-sonnet-4.5` from OpenRouter. Table fragments are therefore always written in the dashed form. Entries are ordered most-specific-first, since some families split: `gpt-5.4` is ~1M but its own `-mini`/`-nano` stayed at 400K.

The numbers come from [models.dev](https://models.dev/api.json) (`.<provider>.models.<id>.limit.context`), the catalog opencode itself resolves against. Within a family the entry is pinned to the *smallest* window that family is served with, so a new sibling lands low rather than high. The Anthropic block is the visible consequence of that rule: every current Claude is natively 1M, but they are enumerated individually and the `claude` family entry stays at 200K, so a release newer than the table is under-budgeted rather than over-budgeted.

That default used to be unconditional, and being wrong by 10x for a frontier model was invisible: nothing logged the budget in force, so the first symptom was a stall warning that read like a bug in the agent loop. Every agent step now states it:

```
INF agent.compaction_budget agent=coder model=google/gemini-2.5-pro compact_after_tokens=800000 context_window=1000000
INF agent.compaction_budget agent=coder model=some-local-build compact_after_tokens=102400 context_window=unknown assumed_window="128000 (set context_window: if your model differs)"
```

**`context_window:` is the escape hatch, and usually the right one.** No table keyed on a model name can express a *host* that serves a known model with a smaller window than it has natively, and none of them will have heard of a local build or a release newer than they are. Stating the window keeps the 80% arithmetic applying and makes the log line above report a derived window instead of an assumed one:

```yaml test=internals-context-window
agents:
- name: reviewer
  source: { model: lmstudio/qwen2.5-coder }
  max_turns: 60
  context_window: 32000    # -> compacts at 25,600

jobs:
- name: review
  plan:
  - agent: reviewer
    prompt: "Review the change."
    assert:
      stdout: Reviewed
  assert:
    execution: [reviewer]
    outcome: succeeded
```

`compact_after_tokens:` still outranks it, and overrides the budget outright rather than describing the model. Prefer `context_window:` unless you specifically want a budget that is not 80% of the window. Neither is available on a `@cli/` agent, which resolves its own window and compacts its own conversation.

**Small-context and local models must set one of the two.** A local build's name tells the table nothing, so it gets the 102,400 default — close to an entire typical context window, and a 32K-token local model (LM Studio, Ollama) will overflow long before that ever triggers.

**The count is a local estimate, not accounting.** It's the same `len(text)/4` heuristic used elsewhere, applied to the conversation's own content — never the provider's real token-usage data. "`steps` tracks no token usage anywhere" (see the OpenRouter section above) still holds; this is a size heuristic that decides *when to compact*, not a usage figure anything reports.

### Tuning one tool's inline budget

The 32,000-byte cap is the default, which is right for most tools but has no answer at either extreme — a tool whose output is mostly noise by construction (a fuzzy search whose tail costs context on every subsequent turn), or one whose output the model genuinely needs whole (a structured report that loses its meaning as a spill pointer). `max_output_bytes:` on a grant sets that one tool's budget in either direction:

```yaml fragment
tools:
- mcp: gopls
  tools: [go_symbol_references]
  max_output_bytes: 6000
```

An explicit value wins over the default, bounded above by the 10MB spill ceiling. Narrowing loses no data — overflow still spills to a file the model can read back — it only shrinks what lands inline; widening is a declared trade of context for completeness.

It is rejected on **built-ins**, which already carry their own output contract (`read_file` pages, `list_dir` counts entries, `search_files` is bounded by arithmetic); stacking a second, conflicting bound on a designed one is a bug surface rather than a knob. It is also rejected on **sub-agent tools**, whose result is another agent's considered answer rather than a data dump. It is valid on custom tools and on all three MCP grant forms — unlike `description`/`required`/`max_calls`, which stay single-`tool:`-only, because the tool worth capping is typically one noisy member of a `tools: [...]` subset and making it carry the cap in its own grant entry would open a second connection to the same server.

`max_output_bytes:` **is** part of the merkle hash (value-gated, so leaving it unset hashes byte-identically to before the field existed) — it changes what a call returns to the model, and therefore the conversation the step produces.

**Compaction is lossy.** Tool results are truncated to 4000 bytes apiece in the summarization prompt, and everything older than the retained window survives only as prose in the summary — not verbatim. Separately, and independently of compaction: any tool result too large to return inline (over 32,000 bytes — `run_shell`/a custom tool, an MCP tool's text or structured content, a sub-agent's final answer, a `fix:` loop's failure output) is instead saved to a file under the step's working directory, with a short `<persistent_file>` pointer message (path, size, a preview) taking its place in the conversation. That pointer message is well under 4000 bytes, so it survives compaction's own truncation intact — a compacted agent can still `read_file` whatever it had already gathered before compaction, even though the raw conversation turn that produced it is gone. `read_file` itself is the exception to the spill-to-file treatment: it reads up to 100,000 bytes in a single call — deliberately larger than the 32,000-byte spill threshold, so a spilled file can always be pulled back whole in one read rather than looping the model on a truncated prefix — and an oversized *file* read degrades to a plain truncation (spilling a file read back out to another file would be a pointless loop) with `start_line`/`end_line` to page through the rest.

**Not part of the merkle hash.** Like `timeout:`/`attempts:`, `compact_after_tokens:` is operational — it changes how a conversation manages its own context budget, not the result it's aiming for — so it never enters a step's hashed content and cannot invalidate a cached step, in either direction.

## Transcript persistence

Every agent step persists two records with different bounds, because they serve different readers:

- **`nodes.result`** carries the bounded summary — response, turn count, verdict/note, and the *trajectory* (tool calls only: name, args truncated to 2,048 bytes per value, ok flag). Planners and routed-to successors load this on every run, so it must stay small.
- **`node_transcripts`** carries the full exchange, keyed by the same node hash: the model's visible text each turn (including commentary accompanying tool calls, which the trajectory drops), every tool call, every tool result (truncated to 4,096 bytes apiece — the model may have seen up to 32,000), and sub-agent delegations with the child conversation's own events nested inside. It is read on demand ("what did this step actually say and do"), never on the run path.

The transcript is saved for **every** outcome — success, run failure, assert failure, capture failure — from one call site in `RunStep`, because a failed step's transcript is the one a human reconstructs from. The write is best-effort on a detached context, like `recordAgentFailure`: an auxiliary record must neither mask the step's own outcome nor be dropped because the step was aborted. Re-recording the same hash replaces the row, matching `nodes`.

Two paths never write one: `RunHook`/`RunFix` (the no-record contract — the enclosing step records the aggregate outcome), and CLI-backed conversations (`cli.go` delegates the loop to a subprocess; its trajectory comes back over the bridge, but there is no in-process turn stream to transcribe).
