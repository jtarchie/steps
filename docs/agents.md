# Agent Steps

How an `agent` step in a pipeline actually runs, and the features around custom tools: required tools, call budgets/pinned args, sub-agent delegation, and reusable tasks. See `examples/agents.yml` for runnable (needs a live LLM) reference jobs.

## The execution flow

An agent step runs a tool-calling conversation loop:

1. Parse the agent's config: model/endpoint, system prompt, granted tools, `max_turns` (default 8).
2. Build a system message combining the agent's persona with working-directory context.
3. Loop, up to `max_turns`:
   - Send the conversation + tool definitions to the model.
   - If the model requests tools, execute them (`read_file`, `list_dir`, `run_shell`, `write_file`, or a custom/sub-agent tool).
   - Truncate any tool output to 100KB before it goes back to the model, so a noisy command can't blow out the context window.
   - Append the tool results and continue.
4. Exit when the model stops requesting tools, or `max_turns` is exceeded.
5. Print the model's final response text to the terminal, followed by its verdict and note if the step declares `verdicts:` — this happens whether the run succeeded or hit its turn budget, since a turn-exhausted attempt's partial response is still available.
6. Record the step's output.

Two tools can be synthesized onto a step's grant beyond what `tools:` lists: a required `verdict` tool (`verdicts:` on the step) and a read-only `previous_run` tool (`handoff: {tool: true}`) — both documented in [control-flow.md](control-flow.md)'s "Step transitions" and "Handoff context" sections, since both exist to serve `to:` routing rather than the tool-calling loop itself.

## Built-in tools

`read_file`, `list_dir`, and `run_shell` are granted automatically whenever a step's `tools:` is absent (`config.DefaultAgentToolSpecs`) — this is the zero-config default every existing pipeline already gets. `write_file` (write, or with `append: true` append, a UTF-8 text file at a path relative to the working directory) is a fourth built-in, but is deliberately **not** part of that default set — folding it in would change the resolved tool grant, and therefore the merkle hash, of every agent step that declares no `tools:` block. To grant it, list it explicitly alongside whichever others you still want:

```yaml
tools: [read_file, list_dir, run_shell, write_file]
```

`write_file` requires the file's immediate parent directory to already exist — it does not create missing directories, matching `read_file`/`list_dir`'s own no-side-effect posture. Use `run_shell` (e.g. `mkdir -p`) first if the directory doesn't exist yet. Like `read_file`/`list_dir`, its path is confined to the working directory and re-validated against a symlink escape (see `resolveWritePath` in `internal/agent/tools.go`), including the case of a symlinked parent directory for a file that doesn't exist yet.

## Working directory, inputs, and dir:

An agent step's `dir:` sets its working directory *and* names the artifact it operates in (its first path component — `dir: repo/cmd` names `repo`), so it's flow-validated for availability like an input. Declaring `inputs:` (a resource an earlier `get` fetched or an output an earlier step produced) does the same. Both are optional, but when you declare one it's checked: an agent pointed at a directory nothing fetched — e.g. "summarize the repository" with no `get` — fails at plan time, before the model is ever called, rather than after burning a turn budget. See [workspace.md](workspace.md) for the full inputs/outputs model.

## Custom tool `required:` semantics

A custom tool (a `tools:` entry with `name`/`description`/`run`) can be marked `required: true` (see `examples/agents.yml`'s `post_review`). This means the step can't complete until that tool has *succeeded* — but no tool failure, required or not, ever aborts or restarts the conversation. A failed call always comes back to the model as ordinary data (`{exit_code, stdout, stderr}` or `{"error": ...}`), exactly like `run_shell`, so the model sees what went wrong and can recover in the same session.

This is enforced in `internal/agent/conversation.go`'s `runAgentConversation`, tracking **success** (`exit_code == 0`), not mere invocation:

- If the model tries to stop before a required tool has succeeded, its next turn is constrained — via the provider's `tool_choice` (`forceRequiredTool`) — to a function call for that specific tool. This is a hard API-level constraint, not a text reminder the model could ignore.
  - Some OpenAI-compatible local servers (LM Studio confirmed; Ollama assumed similar) reject that named-object `tool_choice` form and only accept the strings `none`/`auto`/`required`. `AgentSource.StringToolChoice` (YAML `string_tool_choice:`) picks which form is sent — unset, it defaults to the generic string form for a resolved `lmstudio`/`ollama` provider and the precise named form otherwise. The string fallback only guarantees *some* tool call, not the specific missing one, so `max_turns` is still the real backstop if the model keeps calling the wrong tool.
- A forced (or voluntary) call that still fails is appended to the conversation like any other tool result — the model gets another turn to fix it.
- The safety bound is `max_turns`: if a provider ignores `tool_choice`, or the model just can't get it right, the loop still terminates and the step fails, naming the tool(s) that never succeeded.
- `retry.Do`/`attempts:` (a full conversation restart) only fires for *non-tool* failures — a transport error or `max_turns` exhaustion — never for a tool's own failure.
- Only custom tools can be `required:`. Built-ins (`read_file`, `list_dir`, `run_shell`, `write_file`) and the fix-agent's injected task-rerun tool are always exploratory/iterative.

## Call guards: `max_calls:` and `args:`

A custom tool may also set `max_calls:` (an int) and/or `args:` (a `map[string]string`). Both are rejected at `LoadConfig` on a builtin or sub-agent tool — they only make sense on a custom `run:` command, whose arguments are template-rendered. A negative `max_calls:` is also a load error.

```yaml
tools:
- name: post_review
  description: Post a review verdict. action is approve or comment.
  run: gh pr review --repo {{ .args.repo | shellquote }} --{{ .args.action | shellquote }} -b {{ .args.body | shellquote }}
  required: true
  max_calls: 1          # the model can call this at most once per attempt
  args:
    repo: jtarchie/ci    # pinned — the model neither sees nor can override this
```

- **`args:` pinning** (`internal/agent/tools.go`): a pinned key is excluded entirely from the schema the model sees — it can't author a value for it. At execution, the pinned value is merged in *over* whatever the model supplied at the same key, before rendering `run:` and before the missing-argument check. Values are plain strings, not templated — the model chooses *when*, the pipeline chooses *where*.
- **`max_calls:` budget** (`internal/agent/conversation.go`): a per-tool counter local to one conversation, so it resets on an `attempts:` restart. Once exhausted, the next call is rejected before it ever reaches the tool's implementation (no side effect runs), and comes back to the model as `{"error": "<tool>: call budget (<n>) exhausted for this attempt"}` — same failure-is-data contract as everything else. A budget-rejected call never counts toward satisfying `required: true`; if the budget runs out before a required tool succeeds, the ordinary `max_turns` failure path handles it.
- **Grammar note**: a builtin can be written as a mapping (`{builtin: read_file}`) instead of a bare scalar, purely so `max_calls:`/`args:` have somewhere to attach (and be rejected at load time). Mixing `builtin:` with `name:`/`run:` in one entry is a load error.

## Sub-agent delegation (`agent:` tools)

A `tools:` entry can be a sub-agent tool — `{ agent: <name>, description: <text> }` — instead of a builtin or custom tool. It exposes another `agents:` entry to the parent model as a callable tool taking a single `request` string. This is "delegate and get an answer back," distinct from a job/resource handoff — it only touches `internal/config`, `internal/agent`, and `internal/merkle`.

```yaml
agents:
- name: summarizer
  source: { model: lmstudio/qwen2.5-coder }
  tools: [read_file]
- name: lead
  source: { model: openrouter/anthropic/claude-3.5-sonnet }
  tools:
  - read_file
  - agent: summarizer            # a sub-agent, exposed as a callable tool
    description: Summarize a file; pass the path in `request`.
```

- Each call runs the child's own fresh tool-calling conversation (its own model, persona, dials, `max_turns`, tool grant) and returns its final text as the tool result, capped at 100KB like any other tool output.
- The child runs in the **caller's** working directory but under the **child's own** resolved image — a sub-agent is a different worker, unlike a fix agent (which reproduces the failing task's image; see [infra.md](infra.md)).
- The child's LLM client and tool tree are built eagerly during the parent step's preparation, so a bad credential or grant for a granted sub-agent fails before the first call, not during it.
- No merkle node, `job_run`, or execution-log entry is recorded for the child conversation — same no-record contract as `fix:` agents and hook steps. Only the parent's *call* of the sub-agent tool appears in the parent's own trajectory.
- A child failure (transport error, exhausted `max_turns`, a child required tool never succeeding) comes back to the parent as `{"error": ...}`, never a Go error that aborts the parent conversation.
- A sub-agent is a capability grant like `run_shell`: a step selects a granted one by bare name and cannot introduce one inline.
- **Load-time graph checks**: a sub-agent tool must set no `builtin`/`name`/`run`, can never be `required:`, and must reference an existing agent. The agent graph is walked depth-first for cycles and capped at a nesting depth of 8. A fix agent may not grant sub-agents (out of scope for v1).
- **Merkle**: a sub-agent tool folds in the child's resolved invocation content (model/endpoint/persona/dials/max_turns/image + its own tools, recursively), so editing a child — or grandchild — busts the parent step's hash. The child's prompt/dir/inputs/outputs/assert/hooks aren't part of its identity (a sub-agent has no step), and its API key/env var are excluded.

## Top-level `tasks:` reuse

A top-level `tasks:` list (mirroring `resources:`/`agents:`) lets a `run:`/`fix:` pair be defined once and reused across jobs. A job's `task:` step is disambiguated by whether it carries its own `run:`:

- **`run:` present** → the step is inline; `tasks:` is never consulted, even if a same-named entry exists there.
- **`run:` absent** → `task:` instead names a `tasks:` entry, and its `run`/`fix` are used. The step's own `fix:`, if set, overrides the referenced task's `fix:` for that step only.

This resolution (`Config.ResolveTask`) runs identically at plan time and run time, so a task's merkle hash is always computed from its *resolved* `run:` string — an inline task's hash is unaffected. An undefined reference is an ordinary `FindTask` error at plan time. An agent step's connection/dials/tool-grant resolve the same way, via `Config.ResolveAgentInvocation`.

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
| A `fix:` agent retrying | yes | same persona every attempt |
| Two different agents in one job | **no** | different model and persona — nothing to reuse |
| The same agent in two runs (incl. concurrent) | **no** | no cross-run provider pin |
| A second/third `attempts:` retry | **no** | see below |

A retry deliberately **breaks** the pin instead of extending it. `retry.Do` wraps "a network round trip to an LLM endpoint (rate limiting, transient 5xx)" and retries on any error, so the failure being retried may well be the pinned provider's — reusing the session would send the retry straight back to the instance that just failed. What that gives up is small: a retry starts a *fresh* conversation, so a shared session would only have reused the short system+tools+prompt prefix, never the accumulated history, which is discarded either way.

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

**Timeout** bounds the **entire conversation** (all turns, all tool calls) to a wall-clock deadline. The built-in `agentStepTimeout` is 10 minutes; `timeout:` (if set) overrides it. A timeout mid-conversation classifies as **errored**, not failed.

**Attempts** retries the **whole conversation** on failure (LLM transport error, `max_turns` exhaustion, a tool error in required-tool mode). Each attempt gets its own fresh conversation with the model — prior turns are discarded, the session pin is broken (see OpenRouter section above), and the `max_turns` budget resets. Intermediate tool failures *within* one conversation never trigger a retry; they come back to the model as data. Only after all `attempts:` retries are exhausted does a failure become the step's final outcome.

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

**On by default, at 102,400 tokens (80% of an assumed 128K context window) — unlike every other feature on this page.** An agent that sets no `compact_after_tokens:` still gets compaction; `compact_after_tokens: 0` is what disables it. This is a deliberate exception to the value-gating contract in [CLAUDE.md](../CLAUDE.md) ("absent, its behavior ... byte-identical to before it existed") — merkle hashes are unaffected either way (see below), but the conversation's *behavior* differs for any pipeline whose agent crosses the budget. The 20% headroom below a full 128K window is load-bearing, not padding: the size estimate covers the conversation alone, never the system prompt or the tool schemas resent with every request, so a budget set at the full window would only ever fire after a request had already overflowed.

**Small-context and local models must lower it.** 102,400 tokens is close to an entire typical context window — a 32K-token local model (LM Studio, Ollama) will overflow long before the default ever triggers. Set a budget comfortably under the model's real window, e.g. `compact_after_tokens: 20000` for a 32K model:

```yaml
- name: reviewer
  source: { model: lmstudio/qwen2.5-coder }
  max_turns: 60
  compact_after_tokens: 20000     # the 32K default would never fire in time
```

**The count is a local estimate, not accounting.** It's the same `len(text)/4` heuristic used elsewhere, applied to the conversation's own content — never the provider's real token-usage data. "`steps` tracks no token usage anywhere" (see the OpenRouter section above) still holds; this is a size heuristic that decides *when to compact*, not a usage figure anything reports.

**Compaction is lossy.** Tool results are truncated to 4000 bytes apiece in the summarization prompt, and everything older than the retained window survives only as prose in the summary — not verbatim. A `run_shell`/custom-tool result too large to return inline is instead streamed to a file, with a short pointer message (path, size, a preview) taking its place in the conversation; that pointer message is well under 4000 bytes, so it survives compaction's truncation intact — a compacted agent can still `read_file` whatever it had already gathered before compaction, even though the raw conversation turn that produced it is gone.

**Not part of the merkle hash.** Like `timeout:`/`attempts:`, `compact_after_tokens:` is operational — it changes how a conversation manages its own context budget, not the result it's aiming for — so it never enters a step's hashed content and cannot invalidate a cached step, in either direction.
