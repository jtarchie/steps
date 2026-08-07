# Agent Steps

How an `agent` step in a pipeline actually runs, and the features around custom tools: required tools, call budgets/pinned args, sub-agent delegation, and reusable tasks. See `examples/agents.yml` for runnable (needs a live LLM) reference jobs.

## The execution flow

An agent step runs a tool-calling conversation loop:

1. Parse the agent's config: model/endpoint, system prompt, granted tools, `max_turns` (default 8).
2. Build a system message combining the agent's persona with working-directory context (any `context_paths:` files are delivered as synthetic `read_file` tool results — see below).
3. Loop, up to `max_turns`:
   - Send the conversation + tool definitions to the model.
   - If the model requests tools, execute them (`read_file`, `list_dir`, `search_files`, `run_shell`, `write_file`, `edit_file`, or a custom/sub-agent tool).
   - Cap any tool output at 32,000 bytes before it goes back to the model, so a noisy command can't blow out the context window — output over that is saved to a file under the step's working directory instead of being dropped, with a short pointer message taking its place (see [compaction](agents-internals.md#compacting-long-conversations)); `read_file` is the exception on both counts — it reads up to 100,000 bytes (a spilled file exists precisely so the model can pull it back, so its read budget is deliberately larger than the spill threshold) and degrades to a plain truncation with `start_line`/`end_line` paging rather than spilling a file back out to another file.
   - Append the tool results and continue.
4. Exit when the model stops requesting tools, `max_turns` is exceeded, or [loop detection](agents-internals.md#loop-detection) kills a stuck conversation.
5. Print the model's final response text to the terminal, followed by its verdict and note if the step declares `verdicts:` — this happens whether the run succeeded or hit its turn budget, since a turn-exhausted attempt's partial response is still available.
6. Record the step's output.

Two tools can be synthesized onto a step's grant beyond what `tools:` lists: a required `verdict` tool (`verdicts:` on the step) and a read-only `previous_run` tool (`handoff: {tool: true}`) — both documented in [control-flow.md](control-flow.md)'s "Step transitions" and "Handoff context" sections, since both exist to serve `to:` routing rather than the tool-calling loop itself.

## Built-in tools

`read_file`, `list_dir`, and `run_shell` are granted automatically whenever a step's `tools:` is absent (`config.DefaultAgentToolSpecs`) — this is the zero-config default every existing pipeline already gets. Three more built-ins exist but are deliberately **not** in that default set, because folding any of them in would change the resolved tool grant, and therefore the cache hash, of every agent step that declares no `tools:` block:

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
- `attempts:` retries a failing *request* (see [attempts-timeout.md](attempts-timeout.md)); it never restarts the conversation, and a tool's own failure never triggers it. Nothing except `max_turns` ends a conversation early.
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
- **`max_calls:` budget** (`internal/agent/conversation.go`): a per-tool counter local to one conversation. (It used to reset on an `attempts:` restart; conversations no longer restart, so the budget now simply lasts the whole step.) Once exhausted, the next call is rejected before it ever reaches the tool's implementation (no side effect runs), and comes back to the model as `{"error": "<tool>: call budget (<n>) exhausted for this attempt"}` — same failure-is-data contract as everything else. A budget-rejected call never counts toward satisfying `required: true`; if the budget runs out before a required tool succeeds, the ordinary `max_turns` failure path handles it.
- **Grammar note**: a builtin can be written as a mapping (`{builtin: read_file}`) instead of a bare scalar, purely so `max_calls:`/`args:` have somewhere to attach (and be rejected at load time). Mixing `builtin:` with `name:`/`run:` in one entry is a load error.

## Sub-agent delegation (`agent:` tools)

A `tools:` entry can be a sub-agent tool — `{ agent: <name>, description: <text> }` — instead of a builtin or custom tool. It exposes another `agents:` entry to the parent model as a callable tool taking a single `request` string. This is "delegate and get an answer back," distinct from a job/resource handoff — it only touches `internal/config`, `internal/agent`, and `internal/cache`.

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
- No cache node, `job_run`, or execution-log entry is recorded for the child conversation — same no-record contract as `fix:` agents and hook steps. Only the parent's *call* of the sub-agent tool appears in the parent's own trajectory.
- A child failure (transport error, exhausted `max_turns`, a child required tool never succeeding) comes back to the parent as `{"error": ...}`, never a Go error that aborts the parent conversation.
- A sub-agent is a capability grant like `run_shell`: a step selects a granted one by bare name and cannot introduce one inline.
- **Load-time graph checks**: a sub-agent tool must set no `builtin`/`name`/`run`, can never be `required:`, and must reference an existing agent. The agent graph is walked depth-first for cycles and capped at a nesting depth of 8. A fix agent may not grant sub-agents (out of scope for v1).
- **Caching**: a sub-agent tool folds in the child's resolved invocation content (model/endpoint/persona/dials/max_turns/image + its own tools, recursively), so editing a child — or grandchild — busts the parent step's hash. The child's prompt/dir/inputs/outputs/assert/hooks aren't part of its identity (a sub-agent has no step), and its API key/env var are excluded.

## Self-healing tasks (`fix:`)

`fix:` attaches an agent to a **task** step: if the command exits non-zero, the agent is invoked to repair whatever broke, and then the command runs again. A green run never constructs the agent at all, so a passing pipeline pays nothing.

```yaml
agents:
- name: fixer
  system: You fix failing checks. Make the smallest change that works.
  tools: [read_file, search_files, edit_file, run_shell]

jobs:
- name: build
  plan:
  - get: repo
  - task: test
    run: cd repo && go test ./...
    fix: fixer                 # scalar: just the agent name
```

The mapping form takes per-task overrides:

```yaml
    fix:
      agent: fixer
      prompt: Only fix compile errors; never touch a test assertion.
      dir: repo
      tools: [read_file, edit_file]   # narrow the agent's grant for this task
      attempts: 2
      timeout: 10m
```

**How the loop terminates.** The agent is seeded with the failing command's captured output and given the parent task itself as a zero-arg **rerun tool** (its `run:`, never its `fix:`, so a rerun cannot recurse). It can edit, rerun, and see the new output. When the conversation ends, `steps` runs the command one final time, and *that* exit code decides the step. There is no repeat-until-green loop: one agent conversation, then one verdict. A still-red command fails the step normally, firing `on_failure`.

**What a fix agent needs.** It must be able to edit, and the default tool grant deliberately excludes the write tools, so grant them explicitly — `edit_file` for surgical changes, `search_files` to find the failure site without flooding the conversation with shell grep output.

**Restrictions.** A fix agent may not grant sub-agents, may not use MCP tool grants, and may not set `image:` (it runs in the parent task's image). Its conversation records no cache node or `job_run` of its own — only the parent task's outcome is recorded.

**Caching.** A task with a `fix:` makes its chain uncacheable, since whether it succeeds may depend on what a model did. The run prints `note: <step> makes this chain uncacheable (fix: agent)` at the point that happens.

Worked example: the `self-heal` jobs in [`examples/agents.yml`](../examples/agents.yml).

## Top-level `tasks:` reuse

A top-level `tasks:` list (mirroring `resources:`/`agents:`) lets a `run:`/`fix:` pair be defined once and reused across jobs. A job's `task:` step is disambiguated by whether it carries its own `run:`:

- **`run:` present** → the step is inline; `tasks:` is never consulted, even if a same-named entry exists there.
- **`run:` absent** → `task:` instead names a `tasks:` entry, and its `run`/`fix` are used. The step's own `fix:`, if set, overrides the referenced task's `fix:` for that step only.

This resolution (`Config.ResolveTask`) runs identically at plan time and run time, so a task's cache hash is always computed from its *resolved* `run:` string — an inline task's hash is unaffected (a `run_file:` include resolves before that hash is ever computed; see below). An undefined reference is an ordinary `FindTask` error at plan time. An agent step's connection/dials/tool-grant resolve the same way, via `Config.ResolveAgentInvocation`.

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

Every `*_file:` path is resolved **once, at `LoadConfig` time**, relative to the pipeline YAML's own directory — before `validate()` runs and long before any cache hashing or execution — so everything downstream (`ResolveTask`, `ResolveAgentInvocation`, `TaskNodeContent`, `AgentContentMap`, every executor) sees the resolved text and cannot tell it apart from the same value written inline. Since `TaskNodeContent`/`AgentContentMap` hash `run:`/`prompt:`/`system:` **by value**, editing an included file busts the cache cache exactly like editing an inline value would — for free, with no special-casing anywhere else in the codebase.

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

The artifact named must be declared in the step's own `inputs:` (checked at `LoadConfig`, mirroring how `dir:`'s first path component is validated) and must be read out of the artifact's contents, which are untrusted — the same symlink-aware path confinement `read_file`/`list_dir` use (`resolveAgentPath`) applies here too. This form cannot be resolved at load time: `cache.PlanChains` hashes every step before any `get`'s `in:` has run, so the file doesn't exist yet at plan time. That costs nothing, though — an agent step's chain is already unconditionally unskippable (see "Top-level `tasks:` reuse" above and `internal/cache`'s `planNonGetNode`), so there is no caching to lose by resolving this after plan time.

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

**Caching**: the *paths* (not contents) enter the step's hashed content — the files live inside the workspace, so their content is already chained through the input artifacts' own hashes.

**How it works**: At preparation time, each `context_paths` file is read and confined by `resolveAgentPath`. At conversation start, `buildAgentRequest` prepends a simulated `read_file` tool call + result pair for each path before the user prompt — the same `{"content": …}` response shape the real `read_file` tool uses. This keeps the context visible to the model without consuming a turn or injecting content into the system message.

## Authored handoff notes

Enabled with `handoff: { note: true }`.

Agents in a plan are a relay: each works alone, and the next one starts with none of its predecessor's context. Left to files-by-convention, every agent re-researches what the one before it already knew. `handoff: { note: true }` makes the departing agent write a shift-change report *while it still holds the context*, and delivers it to the next agent automatically.

It is the **forward** half of [`handoff:`](control-flow.md#handoff-context-handoff); `context:`/`tool:` are the backward half, and the two compose on one step.

```yaml
- agent: planner
  handoff: { note: true }   # the entire configuration surface
- agent: coder
  handoff: { note: true }
- task: build-check         # non-agent steps are stepped over
- agent: reviewer           # receives the coder's note
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

**What crosses, and what deliberately does not.** Only the three authored fields plus the computed section. The sender's raw response, its conversation, and its tool-call *arguments* stay behind. That is a security boundary, not tidiness: a response can quote shell or MCP output the receiver has no grant for, and an argument can be an arbitrary model-authored string. So the computed section carries **path arguments of file tools only** — every other tool contributes a name and a count (`run_shell x18`), never its command line — and it counts only calls that **succeeded**, since a `write_file` the model requested but that failed touched nothing and listing it would make the one mechanical section into just another claim. Authored fields have their markdown headings demoted, so a sender cannot forge the computed heading. Every part is size-capped — each authored field, and the computed section too — with a budget that is arithmetic rather than hopeful: three fields plus the computed section plus the header sum to well under the receiver's context-file limit, so a runaway sender can never fail the *innocent* step. An oversized field is truncated inline rather than spilled to a file, because the spill directory belongs to the sending step and is deleted the moment it returns.

The receiver is expected to treat the authored part as claims — the built-in `reviewer` persona states the trust order explicitly (deterministic output > the code > model-authored prose).

**Delivery** reuses the `context_paths` machinery exactly: the note is prepended to the receiver's context paths, so it arrives as a synthetic `read_file` result at conversation start — zero turns, guaranteed presence, and re-readable from disk if the conversation is later compacted. Because the path is re-resolved on *every* dispatch rather than captured once, a `to:`-driven redo of the receiver always picks up the newest note. A missing note (the sender was guard-skipped, or never ran) is skipped silently rather than failing the receiver — the one place this differs from `context_paths`, where a missing file is a hard error because the author named it explicitly.

### Notes across a concurrent block

A note chain reaches into `in_parallel:` and `race:` blocks, in both directions:

```yaml
- agent: planner
  handoff: { note: true }     # broadcast into every branch below
- in_parallel:
    steps:
    - agent: security-reviewer
      handoff: { note: true }  # each branch reports out
    - agent: perf-reviewer
      handoff: { note: true }
- agent: synthesizer          # receives BOTH branch notes, in declaration order
```

**Broadcast (fan-out):** the note pending before a block is delivered to every branch. Safe by construction — a note is read-only, so one report with several readers costs nothing.

**Aggregate (fan-in):** the first agent step after the block receives one note per sending branch, each as its own synthetic `read_file`, ordered by declaration rather than by which branch finished first. They stay separate files rather than being merged into one document because the branch a claim came from is part of the claim: a synthesizer needs to know which reviewer said what.

**Framing.** Every delivered note is wrapped in a randomized `<untrusted-…>` fence and introduced as *data, not instructions*. A fan-in puts several model-authored documents into one conversation at once — the widest injection surface this feature has — and the tag is re-rolled until it does not appear in the content, so a note cannot close the fence and append text that reads as the pipeline's own.

**`race:`** needs no special rule: only the winner's note file exists at delivery time, and an absent note is already skipped by design, so listing every racer resolves to the winner.

A block that sends nothing is **transparent** — a block of tasks between two agents does not break the chain, the same way a single intervening task does not.

Still rejected inside a block: `handoff: { context: true }` and `{ tool: true }`. Those describe arriving via a `to:`/`verdicts:` route, and a branch is not a routable position — nothing can route to it, so the fields would be permanently dead. `across:` is also still rejected outright: every cell answers to the same step name, so all of them would write the same `handoff/<name>.md` and only the last would survive.

**Limits, enforced at load time:**
- Agent steps only, never hooks; the sender needs a later agent step in the same get-segment (a note never crosses a `get` fan-out — each fanned-out build is independent), and the receiver must grant `read_file`.
- Sender names must be unique across the whole segment, branches included — the name is the address, so two branches running the same agent would write the same file.
- **Not supported under `workspace: strategy: copy`/`btrfs`.** Under isolation only *declared outputs* survive a step, so a note written to the build root would be discarded with the sender's workspace. This is a load error rather than a silent loss.
- **No `dir:` on either end.** The note lives in the build root; a step with `dir:` writes it inside a materialized input artifact instead (dirtying it), and a receiver with `dir:` can never reach the root — `../handoff/x.md` is rejected as escaping the working directory. Same reasoning as the isolation rule: a load error beats a silent non-delivery.
- **Sender names must be unique within a segment.** A note is addressed by step name (`handoff/<step>.md`), so two `handoff: { note: true }` steps with the same `agent:` would write the same file.
- `handoff` is a reserved artifact name — an artifact so named would materialize over the note directory. Rejected as a step input/output by `ValidateArtifactName`, and as a *resource* name by the `handoff: { note: true }` load check (a `get` materializes into `<build>/<name>` without passing through that validator on the shared strategy).

**Caching**: the `handoff: { note: true }` declaration and the computed sender name both enter the step's hashed content (they change the tool set and the injected context, respectively). The note's *contents* do not — correctness rests on agent steps being unconditionally unskippable, so a receiving agent always re-runs and re-reads the current note. A `task` step reading `handoff/*.md` would **not** be safe that way: tasks are skippable, and could be skipped against a stale note.

**Relation to `handoff:`**: `handoff:` carries context *backward* along a `to:`/`verdicts:` route (a rejected step learning why, via `previous_run`). `handoff: { note: true }` carries it *forward* along the normal path. They compose — the self-build coder uses both.

## The run context store (`context: write`)

A handoff note is a whole document addressed to one specific successor. The run context store is the other shape: individual named facts, recorded once and readable by every later step in the run.

```yaml
- agent: investigator
  prompt: Investigate why the nightly build failed.
  context: write            # scalar shorthand
- agent: fixer
  prompt: Fix the cause.
  context: { write: true }  # mapping form, same thing
```

`context: write` grants a synthesized **`set_context(key, value)`** tool. The model calls it to record a conclusion:

```json
set_context({ "key": "failure_cause", "value": "flaky DNS in the e2e suite" })
```

Facts are captured through a real tool rather than parsed out of the model's final answer, for the same reason `verdict` and `write_handoff` are: free-text parsing fails silently the moment a model formats its reply differently, and a silently-lost fact is indistinguishable from one the model never learned. Unlike those two, `set_context` is **never required** — the step is offered somewhere to put a fact, not made to produce one. Writing the same key again replaces the previous value; the store answers "what is true now", and the trajectory already records every call that got it there.

**At the tool boundary**, a call is refused — as data the model can react to, never as a step failure:
- keys under the reserved `internal.` prefix, so a model cannot overwrite engine bookkeeping by guessing a name;
- keys outside `[A-Za-z0-9_-.]` or longer than 128 characters, since a key is an identifier a later step reads back by name and whitespace would make one that renders one way and matches another;
- values over 8 KiB — a value is quoted into a later model's context, so an unbounded one spends a downstream step's whole window on a single fact.

### Tasks write context too

A shell command cannot call a tool, so a `context: write` **task** records facts by writing files into a `context/` directory in its working space. The file name is the key, verbatim:

```yaml
- task: run-tests
  inputs: [repo]
  context: write
  run: |
    go test ./... > out.txt 2>&1 || true
    mkdir -p context
    grep -c FAIL out.txt > context/failure_count
    printf 'expired cert' > context/failure_cause
```

The name is used exactly as written — extension included — so a task that wants the key `failure_cause` writes `context/failure_cause`, not `failure_cause.txt`. Stripping extensions would be a quiet rule that makes `a.txt` and `a.md` collide for reasons nobody can see in the pipeline.

A file whose name is not a valid key, or whose contents exceed the value cap, is **skipped with a warning** rather than failing the step: the command already ran and succeeded, and failing it afterwards over the shape of a file name would discard real work for bookkeeping. Nested directories are skipped for the same reason — keys are flat.

`context` is a reserved artifact name, like `handoff`.

**Cache replay.** Tasks are skippable, unlike agent steps — so a task that recorded facts and is later a cache hit would leave a rerun with none of them, and a cached run would disagree with a fresh one about what is true. The recorded facts are therefore stashed on the task's node alongside its outcome, and **replayed when the step is skipped**. Unlike a handoff note, this works under every workspace strategy: the values go to SQLite, not into another step's directory.

**Limits, enforced at load time:**
- Agent and task steps only — a put hands an artifact to a resource and has nothing of its own to say.
- `fidelity:` is agent-only: it renders a recap into a conversation, and a shell command has none.
- Never on a hook step: a hook runs outside the plan's ordering, so what it stored — and whether the steps reading it had already run — would depend on when it happened to fire.
- **Not yet supported inside `in_parallel:`/`race:` or on an `across:` step.** Concurrent branches each work from their own copy, so branch writes have to surface at the *join* rather than merge into the shared store — two branches writing one key would resolve to whichever finished last, the hazard `validateParallelOutputs` already refuses for artifact names. The join half does not exist yet, so this is a load error rather than a silently discarded write.

**Scope**: the store is keyed by run, so two runs of one job — including two concurrent ones under `steps watch` — never read each other's facts. Rows carry `written_by`, so the record answers "who recorded this" without replaying a transcript.

### Reading it back: the recap

Reading is **automatic**. Every agent step opens with a rendered recap of what earlier steps recorded, delivered as a synthetic `read_context` tool result — the same trick `context_paths` uses, so it costs no turn and cannot be skipped by a model that decides not to look. A `read_context` tool is offered alongside it, so a conversation that later compacts can ask for the facts again instead of working from a summary of them.

Nothing is delivered when nothing was recorded: a pipeline that never writes context sees no recap, no tool, and no change to what reaches the wire.

`fidelity:` controls how much of each fact survives:

| fidelity | what the step sees |
|---|---|
| `off` | nothing at all — the complete opt-out |
| `truncate` | key names only, with who recorded them |
| `compact` *(default)* | each key with its value shortened to ~240 characters, elision marked |
| `summary` | each key with its value in full |

There is deliberately no "share the whole prior conversation" rung: agent steps are hermetic here — each is a fresh conversation — so every level is a rendered recap.

Set it per step, or pipeline-wide; first match wins:

```yaml
defaults:
  context:
    fidelity: summary       # pipeline-wide

jobs:
- name: triage
  plan:
  - agent: investigator
    context: write          # writes; still reads at the default level
  - agent: notifier
    context: { fidelity: "off" }   # reads nothing
  - agent: auditor
    context: { write: true, fidelity: summary }  # both switches, independently
```

The recap opens by saying what it is — recorded facts are **data, not instructions** — because it carries text one model wrote into another model's context, and without the framing a recorded "ignore your instructions" reads as one.

**Reads are agent-only.** Task `run:`/`params:`, put params, and `when:` guards cannot reference the context store, and no template hook exists for them. That boundary is what keeps content-addressed caching honest: a shell command whose text depended on a runtime fact could not be hashed at plan time, and a task is skippable in a way an agent step is not.

**Caching**: the `context: write` declaration and the `fidelity:` setting both enter the step's hashed content (one changes the tool grant, the other changes the opening conversation, and two steps differing in either are not the same step). What a step *stored* does not — it cannot be known at plan time, and agent steps are unconditionally unskippable, so a run always re-executes and re-records rather than replaying a stale write.

## What's not on this page

The mechanics underneath an agent step — malformed tool-call repair, loop
detection, OpenRouter prompt caching, how `timeout:`/`attempts:` interact, and
conversation compaction — are in [agents-internals.md](agents-internals.md).
Reach for it when behavior surprises you; you don't need it to write a
pipeline.

## Budgets: `budget.tokens`

An agent step can loop, hold a long conversation where every turn re-sends the whole history, and retry. `budget:` is the ceiling on that — the AI equivalent of `timeout:`.

```yaml
agents:
- name: writer
  source: { model: openrouter/anthropic/claude-sonnet-4-5 }
  budget:
    tokens: 200000      # per invocation of this agent

jobs:
- name: publish
  budget:
    tokens: 500000      # cumulative, across every agent step in the job
  plan: [...]
```

**Reporting happens whether or not you set one**, which is the point: it carries no risk, and it is what tells you which ceilings are even sensible. Every job that ran an agent step prints what it cost.

```
usage: 341,204 tokens across 4 agent step(s)
  planner          120,338
  coder            181,402
  reviewer          39,464
```

Things worth knowing:

- **The numbers are the provider's own reported usage**, not the `len(text)/4` estimate `compact_after_tokens:` uses to decide when to summarize. A provider that reports no usage contributes nothing — a ceiling must never trip on a number nobody reported, so this deliberately does not fall back to an estimate.
- **A breach stops the step before its next tool calls run**, so a step that has already blown its ceiling does not go on to have side effects.
- **A job breach reports the running total per step**, because a cumulative ceiling is rarely tripped by the step that cost the most:
  ```
  job budget exceeded: cap 200000 tokens, spent 200000
    running total: planner 120338 -> coder 79662 (tripped here)
  ```
- **A breach classifies as `errored`, not `failed`.** It is an operational limit being hit, not the model producing a bad answer — so `on_error` fires, `on_failure` does not, and no `to:` route can treat it as a decision the model made. The same treatment timeouts get.
- **Never hashed.** Like `assert:` and `timeout:`, a budget is an operational limit: adding one after reading a usage report must not invalidate every cached step, which would cost exactly what the budget exists to control.
- **A sub-agent has its own budget**, from its own `agents:` entry — it is a separate invocation. Its spend is reported under its own name and rolls into the job total like any other step.

**Tokens only, deliberately.** A token ceiling is provider-agnostic and exact. A money ceiling (`cost: "$2.50"`) would need a per-model price table that goes stale every time any provider changes its rates — an ongoing maintenance burden rather than a one-time cost — so it is left out until someone is prepared to own that table.

## Failover: `fallback:`

When an agent's model is unreachable, try a backup instead of retrying a dead connection.

```yaml
agents:
- name: writer
  source:
    model: openrouter/some/big-model
    api_key_env: PRIMARY_KEY
  fallback:
  - source:
      endpoint: https://backup-provider/v1/
      model: equivalent-model
      api_key_env: BACKUP_KEY
```

This is not hypothetical tidying. In one real run a model went unavailable upstream and killed three consecutive runs over roughly 50 minutes, while `attempts:` amplified the waste rather than absorbing it. Diagnosis took 60 seconds by hand — the same account, key and endpoint served a sibling model in 11 seconds — and the fix was exactly the line above.

How it behaves, and why:

- **Preflight is the trigger.** A primary that fails its pre-run probe (see `steps preflight`) is exactly when to pick an alternate: before the run has spent anything, rather than after failing partway in. Sources are tried in order; the first that answers serves the run.
- **Connection-level failures only.** A probe can only produce a timeout, an unreachable endpoint, or a 5xx. A model *refusing* a request is a different class entirely, already handled by ordinary error handling — falling over on one would silently reroute a legitimate refusal to a possibly less suitable model.
- **The total is bounded.** Each source gets one probe, and `attempts:` applies to requests within whichever source won — not per source. A primary plus two fallbacks is at most three probes, not a multiplying tail of retries across all of them.
- **Only the source changes.** Persona, dials, limits and tool grant are untouched: an outage changes where requests go, never what the agent is or is allowed to do. The compaction budget does follow the fallback model, since a 200K backup must not inherit a 1M primary's budget.
- **Never hashed, with a test.** Which source served a run is *availability*, not content. Declaring a fallback, or having one fire, does not change a step's cache key — the alternative would invalidate every agent step in the pipeline at exactly the moment things are already going badly.
- **Loudly visible.** A fallback can produce meaningfully different output, so a run that used one says so in the log (`agent.failover`), on the step's own output line (`agent: writer (fallback: equivalent-model — big-model is unavailable)`), and in the recorded result (`fallback_model`). A quality dip caused by an outage that looks identical to a normal run is one nobody investigates.
- **Every fallback endpoint is validated** like the primary — no credentials in the URL, and the provider prefix must resolve at load. A fallback nobody can resolve is one that will not save you, discovered during the outage it exists for.

## Ensembles: asking several agents the same question

A single model has blind spots. Ask one reviewer "is this correct?" and you get one opinion with no signal about how much to trust it; ask three and require a majority, and one model's bad day stops being decisive.

```yaml
- ensemble:
    verdicts: [reject, approve]     # the vocabulary EVERY member votes in
    decide: majority                # or: unanimous, any, or an agent name
    member_errors: fail             # or: exclude
    agents:
    - {agent: reviewer-a, prompt: "Review the diff for correctness."}
    - {agent: reviewer-b, prompt: "Review the diff for correctness."}
    - {agent: reviewer-c, prompt: "Review the diff for correctness."}
  to:
    approve: publish
    reject: revise
```

### ⚠️ N agents cost N times one

Three reviewers cost three reviews, every run. This is the step where a job-level `budget:` earns its keep.

### The decision rules

- **`majority`** — the verdict more than half the voters chose.
- **`unanimous`** — every voter agreed, or the block fails saying they did not.
- **`any`** — the first verdict in `verdicts:` that anybody chose. `verdicts:` is an *ordered* list, so listing them most-to-least severe gives you "one objection is enough".
- **an agent name** — that agent judges, receiving every member's vote and note. It is an ordinary agent step, so its reasoning is recorded, inspectable and cached like any other; a judge that is also a voting member is a load error, because it would be marking its own homework.

### Two things that are never silent

- **A tie is an error.** With an even membership, or three verdicts and no majority, picking the first vote would be an invisible bug. Name an agent in `decide:` to break ties deliberately.
- **A member that ERRORS is not a member that voted.** A model or tool failure is not a "reject". By default one failed member fails the block; `member_errors: exclude` decides among the rest, and you have to ask for it — otherwise a three-agent ensemble silently becomes a two-agent one with a different meaning.

### The rest

- Members run **concurrently**, and each is its own merkle node: editing one member's prompt re-runs only that member.
- `verdicts:` and `to:` live on the **block**, not on members. Every member votes in one vocabulary, and the block routes on the *decision* — a member routing on its own vote would leave the block half-taken.
- Every member's vote and note is recorded with the step's result, so a run's record says what was decided *and what it was decided from*.
