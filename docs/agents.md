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
