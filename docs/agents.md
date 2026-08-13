# Agent Steps

How an `agent` step in a pipeline actually runs, and the features around custom tools: required tools, call budgets/pinned args, sub-agent delegation, and reusable tasks. See `examples/agents.yml` for runnable (needs a live LLM) reference jobs.

## The execution flow

An agent step runs a tool-calling conversation loop:

1. Parse the agent's config: model/endpoint, system prompt, granted tools, `max_turns` (default 30; a step may override it with its own `max_turns:` so one long-horizon step can buy more turns without every step of the same agent paying for them).
2. Build a system message combining the agent's persona with working-directory context (any `context_paths:` files are delivered as synthetic `read_file` tool results — see below).
3. Loop, up to `max_turns`:
   - Send the conversation + tool definitions to the model.
   - If the model requests tools, execute them (`read_file`, `list_dir`, `search_files`, `run_shell`, `write_file`, `edit_file`, or a custom/sub-agent tool).
   - Cap any tool output at 32,000 bytes before it goes back to the model, so a noisy command can't blow out the context window — output over that is saved to a file under the step's working directory instead of being dropped, with a short pointer message taking its place (see [compaction](agents-internals.md#compacting-long-conversations)); `read_file` is the exception on both counts — it reads up to 100,000 bytes (a spilled file exists precisely so the model can pull it back, so its read budget is deliberately larger than the spill threshold) and degrades to a plain truncation with `start_line`/`end_line` paging rather than spilling a file back out to another file.
   - Append the tool results and continue.
4. Exit when the model stops requesting tools, `max_turns` is exceeded, or [loop detection](agents-internals.md#loop-detection) kills a stuck conversation.
   - A spent turn budget **ends** the conversation rather than destroying it: the runner makes one final request with the tools withheld, asking the model to answer from what it already gathered, and records the answer with `wrapped_up: true` so a degraded answer is tellable from a confident one. Tools are withheld rather than the model being asked politely to stop, because a model that spent every turn calling tools has already demonstrated it will not.
   - If that final request *itself* fails — a 5xx, or a token ceiling breached by it — the step reports **that** failure, unmarked, so it classifies as `errored` and fires `on_error`. A provider outage on the last request is not the model declining to answer, and the two must not collapse into one message.
5. Print the model's final response text to the terminal, followed by its verdict and note if the step declares `verdicts:` — this happens whether the run succeeded or hit its turn budget, since a turn-exhausted attempt's partial response is still available.
6. Record the step's output.

One tool can be synthesized onto a step's grant beyond what `tools:` lists: a required `verdict` tool (`verdicts:` on the step) — documented in [control-flow.md](control-flow.md)'s "Step transitions" section, since it exists to serve routing rather than the tool-calling loop itself.

## Built-in tools

`read_file`, `list_dir`, and `search_files` are granted automatically whenever a step's `tools:` is absent (`config.DefaultAgentToolSpecs`) — the zero-config default is **read-only**. The three built-ins that mutate state or reach the host are deliberately not in it; each is a capability the pipeline must grant explicitly:

| tool | what it does |
|---|---|
| `run_shell` | Run a shell command in the working directory — unconfined within the step's host or container. |
| `write_file` | Write (or with `append: true`, append) a UTF-8 text file. Replaces a whole file. |
| `edit_file` | Replace an exact string in an existing file — change part of a file without re-emitting it. |

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
- `content` — matching lines **with line numbers**, each capped at 500 bytes. This is where a persona gets `file:line` to cite.
- `count` — matches per file.

Unlike every other tool, `search_files` **never spills**: its bound is arithmetic rather than a truncation applied after the fact. Content matches accumulate against a 28KB byte budget (each match costs its line text plus a fixed overhead allowance), so a saturated result lands under the 32,000-byte inline cap by construction whether the lines were short or maximal — `TestSearchWorstCaseFitsInlineBudget` pins that so the arithmetic keeps holding if the constants are retuned. `head_limit` caps results (default 50 paths / 50 lines, ceilings 200/200) and is clamped to the ceiling; `total` and `truncated` report the true scale, so the answer to a flooded result is a narrower pattern, not a second page. `.git`, `node_modules`, `vendor`, binary files, and files over 2MB are skipped. `**` is supported only as a leading glob segment (`**/*.go`) — `filepath.Match` cannot cross separators, and `**` elsewhere is rejected explicitly rather than silently matching nothing.

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

Paths are relative to the step's working directory and confined to its workspace (`resolveAgentPath`, the same guard the file tools use), so in practice the file lives inside a declared input — `repo/CLAUDE.md` inside the `repo` get. They are read at **run time** (per attempt), which is exactly what distinguishes them from `system_file:`: `system_file:` is the pipeline author's own persona, resolved once at `LoadConfig`; `context_paths:` is content that arrives with a fetched artifact and can change between runs. A missing or escaping file fails the step at preparation, before a token is spent — it is operator-authored config, so a bad one is a loud error, not a surprise mid-conversation. A file that is merely too big (over `max_context_bytes:`, default 100KB) is **truncated** instead, with a note saying so and pointing at `read_file`'s `start_line`/`end_line` to page the rest: the author writes a path, not a size, and `pr/pr.diff` is a correct path that would otherwise start failing the day the pull request under review grew — which is exactly when the review matters.

`context_paths` is a step-level field, not agent-level — the agent definition (`agents:`) has no notion of which inputs are available. It is only valid on `agent` steps and requires `read_file` to be in the tool grant (which it is by default). Sub-agents and fix agents do not inherit the parent step's `context_paths`; the parent is expected to provide all necessary context via the sub-agent's `request` argument or the fix agent's prompt.

`max_context_bytes:` is spelled on **either**, and the step's wins:

```yaml
agents:
- name: reviewer
  max_context_bytes: 100000    # what every step of this agent gets by default

jobs:
- name: review
  plan:
  - agent: reviewer
    context_paths: [pr/pr.diff]
    max_context_bytes: 400000  # ...except this one, which is handed the diff
```

That precedence exists because `context_paths:` is itself step-level: two steps sharing one agent routinely hand it different evidence — a large diff to the reviewer, a small manifest to the gatekeeper — and without a step-level ceiling the only way to give them different ones is to duplicate the whole agent under a second name for the sake of one number. `context_window:` deliberately has no step spelling for the mirror-image reason: it describes the *model*, and the model belongs to the agent.

**In an `across:` matrix**, each entry renders `{{ .vars.<name> }}` per cell, so a cell arrives already holding the code it was assigned instead of spending its first turns navigating to it:

```yaml
- across:
  - var: dim
    values: [api, storage]
  agent: reviewer
  context_paths: ["repo/{{ .vars.dim }}.go"]
  prompt: "Review the {{ .vars.dim }} package"
```

One path per entry, rendered per cell. A `{{ .vars.x }}` naming an axis the matrix does not declare is a **load** error for both matrix spellings, exactly as it is in `prompt:` — the error names the entry (`context_paths[0]`), not just the list.

**Caching**: the *paths* (not contents) enter the step's hashed content — the files live inside the workspace, so their content is already chained through the input artifacts' own hashes. A matrix cell hashes the path it rendered to, which is what makes two cells reviewing different files two different steps.

**How it works**: At preparation time, each `context_paths` file is read and confined by `resolveAgentPath`. At conversation start, `buildAgentRequest` prepends a simulated `read_file` tool call + result pair for each path before the user prompt — the same `{"content": …}` response shape the real `read_file` tool uses. This keeps the context visible to the model without consuming a turn or injecting content into the system message.

## Reading another step's decision (`context: { from: ... }`)

A verdict is the one thing every judging step produces. A classifier that simply falls through, or a shell command that wants to branch on what a model decided, needs a way to ask for it.

`from:` is that ask, and it is declared on the **reader**:

```yaml
- agent: reviewer
  prompt: Review the change.
  verdicts: [approve, revise]      # no routing needed — this one just decides

- agent: editor
  prompt: Apply the review.
  context:
    from:
      reviewer: note               # verdict | note | full

- task: gate
  context:
    from:
      reviewer: verdict
  run: '[ "$(grep ^verdict: upstream/reviewer)" = "verdict: approve" ]'
```

- **Levels.** `verdict` is the name it chose; `note` adds the reason it gave; `full` adds its final response text.
- **The demand creates the obligation.** Asking for a `verdict` costs the sender nothing — a step declaring `verdicts:` already must emit one. Asking for a `note` or `full` makes that sender's note **required**: it joins the verdict tool's required arguments, so the model cannot satisfy the call without writing one. A note nobody demanded is one a model may reasonably skip, and afterwards "chose not to" is indistinguishable from "forgot".
- **Nothing arrives unasked.** No `from:`, no delivery. An agent reader receives each decision as a synthetic `read_step` result at turn zero (like `context_paths:`, no turn spent asking); a task reader receives a file per sender at `upstream/<step>`, since a shell command has no conversation.
- **A sender that has not run yet is simply absent** — no error, nothing delivered. That is what makes the revise loop work: the writer at the top of the loop reads the critic *below* it, gets nothing on the first pass, and gets the verdict that sent it back on every pass after. The question is asked of the run ("has that step decided?") rather than of the route, which is why the two steps need no routing relationship at all.
- **Validated at load**: the named step must exist in the job and must declare `verdicts:` (it has no decision to hand on otherwise), and a step may not read itself. Naming a step that comes *later* in the plan is legal — that is the loop.
- **Trust**: a delivered note or response is upstream model-authored text, so it is fenced as data with a tag that cannot occur inside it, the same treatment every other block of upstream model-authored text gets.
- **Caching**: the `from:` declaration folds into the reading step's hash, and makes a *task* reader's chain unskippable — a cached command never runs, so a task whose `from:` changed must not replay an outcome produced without the decision it now asks for.

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

**Reporting happens whether or not you set one**, which is the point: it carries no risk, and it is what tells you which ceilings are even sensible. Every job that ran an agent step prints what it cost — **and records it**, so the question survives the terminal:

```
$ steps runs pipeline.yml --cost
RUN                 TOKENS   CACHED        COST   STEPS
r-8f2a1c         4,102,338      38%    unpriced       9

$ steps runs pipeline.yml --cost --run r-8f2a1c
STEP                                TOKENS   CACHED   DURATION  FINISH
reviewer [dim=state-mutation]      412,880      61%       1m02s  stop
reviewer [dim=api]               1,204,551      22%      14m30s  length  <-- truncated
```

Three columns there answer questions nothing else could:

- **CACHED** is the only place prompt caching reports whether it worked. The requests carry their headers either way, so without this the feature is faith-based.
- **FINISH** distinguishes a model that had little to say from one that was **cut off** by its output limit. A truncated verdict or JSON body wastes every step downstream of it, and it otherwise reads as an ordinary short answer.
- **COST** says `unpriced` rather than `$0.00`. No provider path reports a dollar figure yet; a zero would say the run was free instead of that nobody priced it. The column exists for when one does — deliberately not computed from a bundled price table, which would go stale every time any provider changed rates.

The served model, reasoning tokens, and the provider's whole raw usage block are recorded too. The last one is future-proofing with a reason: the state schema has no versioning, so a field not captured today could never be backfilled.

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

## CLI-backed agents: `@claude/sonnet`

An agent's `source.model` normally names a hosted model steps calls over HTTP. Prefix it with `@` instead and steps runs a coding-agent CLI as a subprocess:

```yaml
agents:
- name: reviewer
  source:
    model: "@claude/sonnet"     # quotes required -- YAML reserves a leading @
  tools: [read_file, run_shell]
```

The quotes are not stylistic. A leading `@` is a reserved indicator in YAML, so an unquoted value is a parse error before steps ever sees it.

`@claude/sonnet` reads as "the claude CLI, asked for sonnet". The part after the slash is passed through untouched, so anything the CLI accepts for `--model` works.

### What changes, and what doesn't

This is **delegation, not a different transport**. The CLI owns the conversation: its own turn loop, its own tools, its own context window. steps owns everything around it, unchanged — the workspace the process runs in, the merkle hash that decides whether the step runs at all, `timeout:`, the recorded trajectory and response, `assert:`, and `verdicts:`/`to:` routing.

That division is the whole point. You get the CLI's own tooling inside a pipeline that still caches, routes, and fans out.

**Authentication** comes from the CLI's own credential store. The subprocess inherits `HOME`, so a subscription login works with no `api_key_env:` at all. Set `api_key_env:` only if you want a specific key forwarded as `ANTHROPIC_API_KEY`.

### The tool grant becomes the CLI's permissions

Granted built-ins map to the CLI's *native* tools, because a CLI is best at the tools its model was trained against:

| granted built-in | claude CLI tool |
| --- | --- |
| `read_file` | `Read` |
| `list_dir` | `Glob` |
| `run_shell` | `Bash` |
| `write_file` | `Write` |
| `edit_file` | `Edit` |
| `search_files` | `Grep` |

Everything else in the grant — custom `run:` tools, `mcp:` grants, and the synthesized `verdict` tool — reaches the CLI over a loopback MCP server steps starts for the step and tears down after. Those are the *same* implementations a hosted agent runs, so output caps, spilling, MCP tool subsetting and MCP auth all behave identically. Credentials stay in the parent process; nothing reaches the CLI's config but a URL and a single-use token.

Anything not granted is **absent**, not merely unapproved: the grant becomes the CLI's entire built-in surface, so a tool nobody named does not exist for that step. That is deny-by-default, and it is why there is no list of forbidden tools to maintain — a capability this build of steps has never heard of is withheld because it was never granted, rather than surviving because nobody remembered to add it. The CLI's own configured MCP servers are excluded too, so the grant is a limit rather than a suggestion.

### A step is not your session

A CLI agent step runs with **no configuration scopes by default**. Your personal `~/.claude` never applies — no user settings, hooks, plugins, skills, or output styles — and the repo's own `.claude/` scope (its `CLAUDE.md`, settings, hooks) loads only when the agent opts in:

```yaml
agents:
- name: builder
  source: { model: "@claude/sonnet" }
  settings: project      # load the repo's checked-in .claude/ scope
```

This is deliberate. A pipeline whose behavior depends on who ran it is not a pipeline, and a personal `PreToolUse` hook firing inside a step nobody declared it on is a surprise nothing in the YAML would explain. Repo config gets the same treatment as every other capability — declared, not inherited — and the opt-in is hashed, so granting or revoking it invalidates the step's cache. It is also markedly cheaper: dropping user-level config out of the system prompt cut a trivial one-step pipeline from ~76K prompt tokens to ~25K in a measured run.

The tradeoff to know about: steps' path confinement is expressed in the CLI's vocabulary now, and the working directory is the fence rather than per-call validation. A grant including `run_shell` makes that distinction academic anyway — it does on the hosted path too.

### Verdicts are enforced at exit

A hosted agent that tries to finish without its required verdict gets forced into one more call via `tool_choice`. There is no such lever across a process boundary, so the rule moves to the exit instead: **a step that declared `verdicts:` and finished without calling the verdict tool has failed.** The failure is routable, so a `failure:` entry in your `verdicts:` list catches it.

The verdict itself is captured in the parent process the moment the tool is called, over the bridge — the CLI is never trusted to report what it decided.

### `attempts:` resumes the conversation

On the hosted path `attempts:` retries one HTTP request underneath a conversation that survives. A CLI agent gets the same guarantee by a different mechanism: the step names a session up front, and every retry **rejoins** it rather than starting the task over. The retried process is told what went wrong and to continue; it is not handed the original prompt again.

This is not a cost optimization, it is the rule the hosted path already follows. `attempts:` used to restart an agent's conversation there too, and was deliberately removed: the workspace survived a restart but the memory did not, so a retried attempt inherited its own half-finished edits with no recollection of making them. A CLI agent has more of that problem, not less, because it edits more.

Two consequences worth knowing:

- **The turn budget is per step, not per attempt.** `max_turns` counts across the whole conversation, so a retry continues on the remaining budget rather than getting a fresh allowance — the same way a request retry on the hosted path never refunds turns. One caveat: turns spent by an attempt that died *before* reporting a result cannot be counted, because the CLI only reports its turn count in the terminal event.
- **The transcript is cleaned up.** Session persistence has to stay on for a retry to resume, but steps deletes the step's own session file afterwards rather than leaving one behind per agent step in your home directory.

Only *infrastructure* failures are retried — the process failed to start, exited nonzero, or died without reporting a result. A CLI that ran fine and concluded the task failed is an answer, not an outage, and is not re-rolled.

### What a CLI agent cannot do

These are load errors, not silent no-ops, because a setting that reads as configured while binding nothing is worse than one that is rejected:

| rejected | why |
| --- | --- |
| `source.endpoint:` | there is no request to aim anywhere |
| `temperature:`, `top_p:`, `max_tokens:`, `reasoning_effort:` | the CLI chooses its own sampling |
| `source.string_tool_choice:` | no `tool_choice` on the wire to spell |
| `compact_after_tokens:`, `context_window:` | the CLI compacts its own conversation, against a window it resolves itself |
| `budget.tokens:` | nothing counts tokens until the subprocess exits (use `budget.usd:`) |
| `required:`, `max_calls:`, `args:` on a tool | enforced by the turn loop the CLI replaces |
| sub-agent tools, in either direction | a sub-agent nests inside a turn loop there is none of |
| a CLI agent as a task's `fix:` agent | same reason |
| `network: none` together with `image:` | the CLI reaches its steps-provided tools (the verdict tool among them) over a connection back to this process |

### Containerizing a CLI agent

`image:` **is** supported, and it does more here than for a hosted agent: it containerizes the CLI process itself, so its native tools (`Read`, `Bash`, `Edit`) are confined to the container rather than running on the host with only the working directory as a fence. Credentials are the part that needs a decision — a Linux subscription login is bind-mounted in read-only, but macOS keeps it in the Keychain where no container can reach it, so `source.api_key_env:` is the portable answer. See [infra.md](infra.md) for the mechanics and `examples/cli-agent-image.yml` for a worked example.

### Budgets are in dollars

A CLI agent takes `budget: {usd: 0.50}` rather than `budget: {tokens:}`. The two runners meter different things and neither converts into the other honestly: a hosted conversation is driven here, so tokens are counted exactly as the provider reports them, while a CLI meters itself in dollars and can stop mid-conversation. Converting between them would need a per-model price table that goes stale whenever any provider changes its rates — a number that would silently go wrong — so each runner takes the unit it can enforce and the other spelling is a load error.

A job-level `budget:` stays in tokens, since it is cumulative across mixed step kinds, and it still counts what a CLI agent spent — the CLI reports its usage on exit, and it is folded into the job total.

`fallback:` works in both directions. A CLI agent can fall back to a hosted provider (useful when the binary is not installed on some machines), and a hosted agent can fall back to a CLI. Preflight checks a CLI target by looking for its binary on `PATH`; `steps preflight` reports a missing one before the run starts. A *containerized* CLI target is checked differently, since the host's `PATH` says nothing about it: preflight runs `docker run --rm <image> claude --version` and checks that credentials are reachable by at least one route.

## Ensembles: asking several agents the same question

A single model has blind spots. Ask one reviewer "is this correct?" and you get one opinion with no signal about how much to trust it; ask three and require a majority, and one model's bad day stops being decisive.

```yaml
- ensemble:
    verdicts:                       # the vocabulary EVERY member votes in,
      - reject: revise              # and where the BLOCK's decision goes
      - approve: publish
    decide: majority                # or: unanimous, any, or an agent name
    member_errors: fail             # or: exclude
    agents:
    - {agent: reviewer-a, prompt: "Review the diff for correctness."}
    - {agent: reviewer-b, prompt: "Review the diff for correctness."}
    - {agent: reviewer-c, prompt: "Review the diff for correctness."}
```

### ⚠️ N agents cost N times one

Three reviewers cost three reviews, every run. This is the step where a job-level `budget:` earns its keep.

### The decision rules

- **`majority`** — the verdict more than half the voters chose.
- **`unanimous`** — every voter agreed, or the block fails saying they did not.
- **`any`** — the first verdict in `verdicts:` that anybody chose. `verdicts:` is an *ordered* list, so listing them most-to-least severe gives you "one objection is enough". (This precedence is one of the two reasons verdict targets live in the list rather than a `to:` map — a map has no order.)
- **an agent name** — that agent judges, receiving every member's vote and note. It is an ordinary agent step, so its reasoning is recorded, inspectable and cached like any other; a judge that is also a voting member is a load error, because it would be marking its own homework.

### Two things that are never silent

- **A tie is an error.** With an even membership, or three verdicts and no majority, picking the first vote would be an invisible bug. Name an agent in `decide:` to break ties deliberately.
- **A member that ERRORS is not a member that voted.** A model or tool failure is not a "reject". By default one failed member fails the block; `member_errors: exclude` decides among the rest, and you have to ask for it — otherwise a three-agent ensemble silently becomes a two-agent one with a different meaning.

### The rest

- Members run **concurrently**, and each is its own merkle node: editing one member's prompt re-runs only that member.
- `verdicts:` lives on the **block**, not on members: it carries both the vocabulary and the block's routing. Every member votes in one vocabulary, and the block routes on the *decision* — a member routing on its own vote would leave the block half-taken. Members are handed the names only, minus the reserved `failure:` catch, which no model may choose.
- Every member's vote and note is recorded with the step's result, so a run's record says what was decided *and what it was decided from*.
