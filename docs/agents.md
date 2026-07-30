# Agent Steps

How an `agent` step in a pipeline actually runs, and the features around custom tools: required tools, call budgets/pinned args, sub-agent delegation, and reusable tasks. See `examples/agents.yml` for runnable (needs a live LLM) reference jobs.

## The execution flow

An agent step runs a tool-calling conversation loop:

1. Parse the agent's config: model/endpoint, system prompt, granted tools, `max_turns` (default 8).
2. Build a system message combining the agent's persona with working-directory context (any `context_paths:` files are delivered as synthetic `read_file` tool results — see below).
3. Loop, up to `max_turns`:
   - Send the conversation + tool definitions to the model.
   - If the model requests tools, execute them (`read_file`, `list_dir`, `search_files`, `run_shell`, `write_file`, `edit_file`, or a custom/sub-agent tool).
   - Cap any tool output at 32,000 bytes before it goes back to the model, so a noisy command can't blow out the context window — output over that is saved to a file under the step's working directory instead of being dropped, with a short pointer message taking its place (see "Compacting long conversations" below); `read_file` is the exception on both counts — it reads up to 100,000 bytes (a spilled file exists precisely so the model can pull it back, so its read budget is deliberately larger than the spill threshold) and degrades to a plain truncation with `start_line`/`end_line` paging rather than spilling a file back out to another file.
   - Append the tool results and continue.
4. Exit when the model stops requesting tools, `max_turns` is exceeded, or loop detection kills a stuck conversation (see "Loop detection" below).
5. Print the model's final response text to the terminal, followed by its verdict and note if the step declares `verdicts:` — this happens whether the run succeeded or hit its turn budget, since a turn-exhausted attempt's partial response is still available.
6. Record the step's output.

Two tools can be synthesized onto a step's grant beyond what `tools:` lists: a required `verdict` tool (`verdicts:` on the step) and a read-only `previous_run` tool (`handoff: {tool: true}`) — both documented in [control-flow.md](control-flow.md)'s "Step transitions" and "Handoff context" sections, since both exist to serve `to:` routing rather than the tool-calling loop itself.

## Built-in tools

`read_file`, `list_dir`, and `run_shell` are granted automatically whenever a step's `tools:` is absent (`config.DefaultAgentToolSpecs`) — this is the zero-config default every existing pipeline already gets. Three more built-ins exist but are deliberately **not** in that default set, because folding any of them in would change the resolved tool grant, and therefore the merkle hash, of every agent step that declares no `tools:` block:

| tool | what it does |
|---|---|
| `write_file` | Write (or with `append: true`, append) a UTF-8 text file. Replaces a whole file. |
| `edit_file` | Replace an exact string in an existing file — change part of a file without re-emitting it. |
| `search_files` | Search file contents by regex and/or paths by glob, with a hard result cap. |

To grant them, list them explicitly alongside whichever others you still want:

```yaml
tools: [read_file, list_dir, search_files, run_shell, write_file, edit_file]
```

### `edit_file`

Takes `path`, `old_string`, `new_string`, and optional `replace_all`. `old_string` must match **exactly once** unless `replace_all` is set; zero matches and ambiguous matches are both returned as errors phrased as next-turn instructions ("read the file again and copy the text exactly", "include more surrounding lines"), since both are recoverable without burning an attempt. The file's mode is preserved, so editing a checked-in script does not strip its executable bit. Returns `replacements`, `first_line`, and `match_mode`, never content.

Matching is forgiving, in three strategies tried in order of decreasing exactness (`internal/agent/editfile.go`, ported from opencode's edit tool): **exact** first; then **line-trimmed** (every line matches modulo leading/trailing whitespace — recovers the classic local-model miss of right block, wrong indentation); then **block-anchor** (for a block of 3+ lines, the first and last lines anchor and the middle is judged by per-line similarity — recovers a misquoted interior line). The matched span is always the file's *own* text, so a forgiving match never rewrites untouched lines to the model's spelling, and a span far larger than the `old_string` that produced it is refused outright. `match_mode` in the result says which strategy landed (`exact`, `line-trimmed`, `block-anchor`) — an inexact edit is visible, not silent, and the tool description tells the model to re-read around `first_line` when it isn't `exact`. A verbatim copy from `read_file` still lands as `exact` every time.

`edit_file` pairs with `read_file` by design: `read_file` returns **raw bytes**, so text copied out of one is a byte-exact `old_string`. Do not add line numbers to `read_file` — it would break every edit a model constructs this way. Line numbers come from `search_files`' `content` mode instead.

### `search_files`

Supply `pattern` (a regexp matched against each line), `glob` (a shell pattern matched against a file's path), or both; `glob` alone is a filename search, so there is no separate glob tool. `path` defaults to `"."`. Three `output_mode`s:

- `files_with_matches` (default) — just the paths, cheapest; read the ones you want with `read_file`.
- `content` — matching lines **with line numbers**, each capped at 200 bytes. This is where a persona gets `file:line` to cite.
- `count` — matches per file.

Unlike every other tool, `search_files` **never spills**: its bound is arithmetic rather than a truncation applied after the fact. A fully saturated `content` result is 80 matches × 200 bytes plus scaffolding — roughly 25KB against the 32,000-byte inline cap, and `TestSearchWorstCaseFitsInlineBudget` pins that so the arithmetic keeps holding if the constants are retuned. `head_limit` caps results (default 50 paths / 30 lines) and is clamped to the ceiling; `total` and `truncated` report the true scale, so the answer to a flooded result is a narrower pattern, not a second page. `.git`, `node_modules`, `vendor`, binary files, and files over 2MB are skipped. `**` is supported only as a leading glob segment (`**/*.go`) — `filepath.Match` cannot cross separators, and `**` elsewhere is rejected explicitly rather than silently matching nothing.

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

- Each call runs the child's own fresh tool-calling conversation (its own model, persona, dials, `max_turns`, tool grant) and returns its final text as the tool result, capped at 32,000 bytes like any other tool output — a chattier answer is saved to a file instead of being dropped, with a pointer message in its place.
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

This resolution (`Config.ResolveTask`) runs identically at plan time and run time, so a task's merkle hash is always computed from its *resolved* `run:` string — an inline task's hash is unaffected (a `run_file:` include resolves before that hash is ever computed; see below). An undefined reference is an ordinary `FindTask` error at plan time. An agent step's connection/dials/tool-grant resolve the same way, via `Config.ResolveAgentInvocation`.

## External files: `run_file:`, `system_file:`, `prompt_file:`, and `file:`

A task's `run:`, an agent's `system:` persona, an agent step's `prompt:`, and a `fix:`'s `prompt:` can all be loaded from a file instead of written inline — useful since a persona or prompt is often long freeform prose, and a `run:` a full shell program:

```yaml
tasks:
- name: unit
  run_file: ci/tasks/unit.sh      # loads Task.Run from a file

agents:
- name: reviewer
  source: { model: openrouter/anthropic/claude-3.5-sonnet }
  system_file: prompts/reviewer.md  # loads Agent.System from a file

jobs:
- name: build
  plan:
  - agent: reviewer
    prompt_file: prompts/review.md  # loads Step.Prompt from a file
```

Every `*_file:` path is resolved **once, at `LoadConfig` time**, relative to the pipeline YAML's own directory — before `validate()` runs and long before any merkle hashing or execution — so everything downstream (`ResolveTask`, `ResolveAgentInvocation`, `TaskNodeContent`, `AgentContentMap`, every executor) sees the resolved text and cannot tell it apart from the same value written inline. Since `TaskNodeContent`/`AgentContentMap` hash `run:`/`prompt:`/`system:` **by value**, editing an included file busts the merkle cache exactly like editing an inline value would — for free, with no special-casing anywhere else in the codebase.

A path may use `..` to escape the pipeline's own directory: the pipeline file is trusted input, and a file placed beside it by the same author is at the same trust level — a shared `../tasks/` directory next to a `pipelines/` directory is a legitimate layout, not a hole to close. Setting both a field and its `*_file:` sibling (e.g. both `run:` and `run_file:`) is a load-time error, and so is an empty included file — either would silently change what the entry means (an empty `run_file:` would leave a task step's `run:` empty, making it fall through `ResolveTask`'s inline short-circuit to a `tasks:` reference instead).

A top-level `tasks:`/`agents:` entry additionally accepts a whole-document `file:`, loading a complete `Task`/`Agent` definition from a separate YAML file so it can be shared across pipelines:

```yaml
tasks:
- name: unit
  file: ci/tasks/unit.yml   # supplies run/fix/image/timeout/inputs/outputs
  image: golang:1.26        # any field set here overrides the document's
```

The entry's own inline fields win over the loaded document's — the same "wins when set" idiom `ResolveTask` already uses between a step and its `tasks:` entry — and the loaded document may not itself use `file:`/`run_file:` (or `file:`/`system_file:` for an agent): includes are resolved one level deep only, which is what makes cycle detection unnecessary.

### The run-time form: an agent step's `prompt_file:` from a fetched artifact

An agent step's `prompt_file:` additionally accepts a `{artifact, path}` mapping, naming a file inside an artifact a `get` step fetched, read at **run time** rather than load time:

```yaml
jobs:
- name: review
  plan:
  - get: repo
    trigger: true
  - agent: reviewer
    inputs: [repo]
    prompt_file: { artifact: repo, path: .ci/REVIEW.md }
```

This is the one place a step's config can come from a fetched artifact, and it is deliberately narrow — task `run:` and a whole agent definition/persona cannot. Two reasons:

- **A task's `run:` already reaches into a fetched artifact today.** `run: sh repo/ci/build.sh` works unchanged in both shared and isolated mode (isolated mode just needs `inputs: [repo]`), so an artifact-sourced task-config file would add nothing beyond what plain `run:` already does, while requiring the step to redeclare its own `inputs:`/`outputs:`/`image:` anyway.
- **An agent's connection is a credential boundary a fetched repo must never cross.** An `Agent`'s `source.endpoint:`/`api_key_env:` decide where a configured API key gets sent; letting a repo supply either would let it redirect that credential to an attacker-chosen server, walking straight around the `HostEnv()` allowlist and `validateAgentEndpoints` that exist to keep exactly that from happening. A prompt is just the task text the model already reads the repo to act on — no new credential exposure.

The artifact named must be declared in the step's own `inputs:` (checked at `LoadConfig`, mirroring how `dir:`'s first path component is validated) and must be read out of the artifact's contents, which are untrusted — the same symlink-aware path confinement `read_file`/`list_dir` use (`resolveAgentPath`) applies here too. This form cannot be resolved at load time: `merkle.PlanChains` hashes every step before any `get`'s `in:` has run, so the file doesn't exist yet at plan time. That costs nothing, though — an agent step's chain is already unconditionally unskippable (see "Top-level `tasks:` reuse" above and `internal/merkle`'s `planNonGetNode`), so there is no caching to lose by resolving this after plan time.

## `context_paths:` files delivered as synthetic `read_file` results

An `agent` step can declare `context_paths:` — files whose contents are injected at conversation start as synthetic `read_file` tool results. The model sees the file contents as if it had called `read_file` itself, without consuming a turn:

```yaml
jobs:
- name: build
  plan:
  - get: repo
  - agent: coder
    inputs: [repo]
    context_paths: [repo/CLAUDE.md]
```

The point is not convenience but **guarantee**: conventions every invocation must follow (build/lint/test commands, package-dependency rules) are present from the first turn, instead of costing a `read_file` round trip the model might not bother with.

Paths are relative to the step's working directory and confined to its workspace (`resolveAgentPath`, the same guard the file tools use), so in practice the file lives inside a declared input — `repo/CLAUDE.md` inside the `repo` get. They are read at **run time** (per attempt), which is exactly what distinguishes them from `system_file:`: `system_file:` is the pipeline author's own persona, resolved once at `LoadConfig`; `context_paths:` is content that arrives with a fetched artifact and can change between runs. A missing, escaping, or over-100KB file fails the step at preparation, before a token is spent — it is operator-authored config, so a bad one is a loud error, not a surprise mid-conversation.

`context_paths` is a step-level field, not agent-level — the agent definition (`agents:`) has no notion of which inputs are available. It is only valid on `agent` steps and requires `read_file` to be in the tool grant (which it is by default). Sub-agents and fix agents do not inherit the parent step's `context_paths`; the parent is expected to provide all necessary context via the sub-agent's `request` argument or the fix agent's prompt.

**Merkle**: the *paths* (not contents) enter the step's hashed content — the files live inside the workspace, so their content is already chained through the input artifacts' own hashes.

**How it works**: At preparation time, each `context_paths` file is read and confined by `resolveAgentPath`. At conversation start, `buildAgentRequest` prepends a simulated `read_file` tool call + result pair for each path before the user prompt — the same `{"content": …}` response shape the real `read_file` tool uses. This keeps the context visible to the model without consuming a turn or injecting content into the system message.

## Authored handoff (`handoff_note:`)

Agents in a plan are a relay: each works alone, and the next one starts with none of its predecessor's context. Left to files-by-convention, every agent re-researches what the one before it already knew. `handoff_note: true` makes the departing agent write a shift-change report *while it still holds the context*, and delivers it to the next agent automatically.

```yaml
- agent: planner
  handoff_note: true      # the entire configuration surface
- agent: coder
  handoff_note: true
- task: build-check       # non-agent steps are stepped over
- agent: reviewer         # receives the coder's note
```

The receiver is **computed, not declared**: the next `agent` step in the same get-segment. There is nothing else to configure — no schema, no addressing, no opt-in on the receiving side.

**The form is fixed** (three required string fields, owned by `steps`, not the pipeline):

| field | what it asks for |
|---|---|
| `done` | factual inventory of what was done, with file:line — explicitly *not* a self-grade |
| `facts` | what the next agent needs, including **dead ends** — what was read and ruled out, so the search isn't repeated |
| `watch_out` | risks, uncertainties, and any deviation from instructions, with the reason |

A synthesized **required** `write_handoff` tool carries them. Required means the same thing it does for `verdicts:`: the model's turn is constrained via `tool_choice` when it tries to finish without having called it (see `forceRequiredTool`), so the note cannot be silently skipped. A step may require both a verdict and a note — the required-tool machinery forces one per turn until all are satisfied.

The rendered note (`handoff/<step>.md` in the build workspace) opens with a provenance header and ends with a section the author could not have written:

```markdown
> Model-authored by agent "coder" (job "self-build") — claims to verify, not facts.
> The final section is computed by the runner and is the only part the author could not write.

## done
...
## Files touched (computed from the run, not authored)
read_file: internal/pipeline/pipeline.go, internal/config/config.go
edit_file: internal/pipeline/pipeline.go
other tools: run_shell x18, verify_gate x11
```

**What crosses, and what deliberately does not.** Only the three authored fields plus the computed section. The sender's raw response, its conversation, and its tool-call *arguments* stay behind. That is a security boundary, not tidiness: a response can quote shell or MCP output the receiver has no grant for, and an argument can be an arbitrary model-authored string. So the computed section carries **path arguments of file tools only** — every other tool contributes a name and a count (`run_shell x18`), never its command line — and it counts only calls that **succeeded**, since a `write_file` the model requested but that failed touched nothing and listing it would make the one mechanical section into just another claim. Authored fields have their markdown headings demoted, so a sender cannot forge the computed heading, and are size-capped so a runaway field cannot push the note past the receiver's context-file limit and fail the *innocent* step.

The receiver is expected to treat the authored part as claims — the built-in `reviewer` persona states the trust order explicitly (deterministic output > the code > model-authored prose).

**Delivery** reuses the `context_paths` machinery exactly: the note is prepended to the receiver's context paths, so it arrives as a synthetic `read_file` result at conversation start — zero turns, guaranteed presence, and re-readable from disk if the conversation is later compacted. Because the path is re-resolved on *every* dispatch rather than captured once, a `to:`-driven redo of the receiver always picks up the newest note. A missing note (the sender was guard-skipped, or never ran) is skipped silently rather than failing the receiver — the one place this differs from `context_paths`, where a missing file is a hard error because the author named it explicitly.

**Limits, enforced at load time:**
- Agent steps only, never hooks; the sender needs a later agent step in the same get-segment (a note never crosses a `get` fan-out — each fanned-out build is independent), and the receiver must grant `read_file`.
- **Not supported under `workspace: strategy: copy`/`btrfs`.** Under isolation only *declared outputs* survive a step, so a note written to the build root would be discarded with the sender's workspace. This is a load error rather than a silent loss.
- `handoff` is a reserved artifact name — an artifact so named would materialize over the note directory.

**Merkle**: the `handoff_note:` declaration and the computed sender name both enter the step's hashed content (they change the tool set and the injected context, respectively). The note's *contents* do not — correctness rests on agent steps being unconditionally unskippable, so a receiving agent always re-runs and re-reads the current note. A `task` step reading `handoff/*.md` would **not** be safe that way: tasks are skippable, and could be skipped against a stale note.

**Relation to `handoff:`**: `handoff:` carries context *backward* along a `to:`/`verdicts:` route (a rejected step learning why, via `previous_run`). `handoff_note:` carries it *forward* along the normal path. They compose — the self-build coder uses both.

## Repairing malformed tool-call arguments

An OpenAI-compatible server sends a tool call's `arguments` as a JSON *string*, and weak local models (LM Studio, Ollama class) malformed it often: truncated at `max_tokens` mid-string, trailing commas, prose or markdown fences around the object. The LLM adapter parses that string leniently — on *any* parse failure it silently substitutes an **empty map**, so the call arrived at the conversation as if the model had passed no arguments at all: the tool answered "missing required argument", the model had no idea why, and a coding agent could burn most of its turn budget rediscovering the failure.

Every agent's HTTP client therefore carries a **response-repair transport** (`internal/agent/repair.go`) that inspects each chat-completion response and, for any `arguments` string that doesn't parse, attempts a minimal best-effort repair before the adapter ever sees it — the same validate → repair → re-validate shape as crush/fantasy's tool-call repair, placed at the transport because that is the last place the model's raw text exists. The rule set covers only what local models actually produce: dropping prose/fences around the object, closing an unterminated string and unclosed braces (truncation), supplying `null` for a dangling key, and stripping trailing commas. Anything else is left exactly as-is — an unrepairable call behaves precisely as it did before this existed, so repair can only recover a call, never alter a valid one (an all-valid response passes through byte-identically, and non-chat requests are never touched).

Transport-level only, like the OpenRouter caching levers: no config surface, no merkle impact, and it applies to **every** provider, since malformed arguments are an OpenAI-compat-wide hazard rather than a provider-specific one.

## Loop detection

`max_turns` bounds how long a conversation can run, but it is a blunt backstop: an agent that re-issues the *identical* tool call and gets the *identical* result — re-reading a file it has already read, re-running a command whose output cannot change — is stuck, and before loop detection it would spin out its entire turn budget producing nothing. The conversation loop now hashes each tool **interaction** (tool name + arguments + result; `internal/agent/loop.go`, copied from crush's `loop_detection.go`) and counts repeats within a sliding window of the last 10 tool-executing turns.

The result is part of the hash so that *productive* repetition never trips the detector: re-reading a file after an edit returns different bytes, and a verify command's output changes as the tree is fixed. Only call-and-result-both-identical accumulates.

The reaction is two-strike. The first time one interaction exceeds 5 copies in the window, the conversation gets a warning message naming the tool and telling the model to change approach — the window is **not** reset, so the warning is the model's only chance. A second detection (i.e. it repeated the same interaction once more) fails the attempt as a task failure (`failed`, not `errored`, for hook dispatch) with "agent stuck in a loop". Unlike crush's window — which only starts counting after 10 full turns — any count over the threshold triggers, so an agent with the default 8 `max_turns` is protected exactly like one with 200. A retry (`attempts:`) starts with a clean detector, like all per-attempt state.

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
