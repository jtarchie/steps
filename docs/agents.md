# Agent Steps

How an `agent` step in a pipeline actually runs, and the features around custom tools: required tools, call budgets/pinned args, sub-agent delegation, and reusable tasks.

## The execution flow

An agent step runs a tool-calling conversation loop:

1. Parse the agent's config: model/endpoint, system prompt, granted tools, `max_turns` (default 30, `0` for no cap; a step may override it with its own `max_turns:` so one long-horizon step can buy more turns without every step of the same agent paying for them). Same for `timeout:` and `attempts:`, which an `agents:` entry may also carry — see [attempts-timeout.md](attempts-timeout.md).
2. Build a system message combining the agent's persona with working-directory context (any `context_paths:` files are delivered as synthetic `read_file` tool results — see below).
3. Loop, up to `max_turns`:
   - Send the conversation + tool definitions to the model.
   - If the model requests tools, execute them (`read_file`, `list_dir`, `search_files`, `run_shell`, `write_file`, `edit_file`, `web_fetch`, or a custom/sub-agent tool).
   - Cap any tool output at 32,000 bytes before it goes back to the model — output over that is saved to a file under the step's working directory instead of being dropped, with a short pointer message taking its place (see [compaction](agents-internals.md#compacting-long-conversations)). Two tools carry their own bound instead, both at 100,000 bytes and neither spilling: `read_file` (a spilled file exists precisely so the model can pull it back), degrading to plain truncation with `start_line`/`end_line` paging, and `web_fetch`, which cuts the body off and says so — the page is still on the web, so a narrower URL is the way to the rest.
   - Append the tool results and continue.
4. Exit when the model stops requesting tools, `max_turns` is exceeded, or [loop detection](agents-internals.md#loop-detection) kills a stuck conversation.
   - A spent turn budget **ends** the conversation rather than destroying it: the runner makes one final request with the tools withheld, asking the model to answer from what it already gathered, and records the answer with `wrapped_up: true` so a degraded answer is tellable from a confident one.
   - If that final request *itself* fails — a 5xx, or a token ceiling breached by it — the step reports **that** failure, unmarked, so it classifies as `errored` and fires `on_error`.
5. Print the model's final response text to the terminal, followed by its verdict and note if the step declares `verdicts:`.
6. Record the step's output.

One tool can be synthesized onto a step's grant beyond what `tools:` lists: a required `verdict` tool (`verdicts:` on the step) — documented in [control-flow.md](control-flow.md#step-transitions-tomax_visitsverdicts), since it exists to serve routing.

## Built-in tools

`read_file`, `list_dir`, and `search_files` are granted automatically whenever `tools:` is absent — the zero-config default is **read-only**:

```yaml test=agents-readonly
agents:
- name: reader
  source: { model: openrouter/qwen/qwen3.7-flash }
  # no tools: -> read_file, list_dir, search_files

jobs:
- name: inspect
  plan:
  - task: fetch
    outputs: [notes]
    run: echo 'widgets ship on tuesday' > notes/plan.txt
  - agent: reader
    inputs: [notes]
    prompt: "What does notes/plan.txt say?"
    assert:
      tool_calls:                 # read_file really ran, with this path...
      - name: read_file
        args: { path: notes/plan.txt }
      stdout: widgets ship on tuesday   # ...and the answer could only come from what it returned
  assert:
    execution: [fetch, reader]
    outcome: succeeded
```

The built-ins that mutate state or reach beyond the workspace are deliberately not in the default; each is a capability the pipeline must grant explicitly:

| tool | what it does |
|---|---|
| `run_shell` | Run a shell command in the working directory — unconfined within the step's host or container. |
| `write_file` | Write (or with `append: true`, append) a UTF-8 text file. Replaces a whole file. |
| `edit_file` | Replace an exact string in an existing file — change part of a file without re-emitting it. |
| `web_fetch` | HTTP GET a URL and return its body — optionally fenced to named hosts with `allow:`. |

```yaml test=agents-writer
agents:
- name: writer
  source: { model: openrouter/qwen/qwen3.7-flash }
  tools: [read_file, write_file, edit_file, run_shell]

jobs:
- name: draft
  plan:
  - agent: writer
    outputs: [report]            # an empty report/ dir exists from turn one
    max_turns: 40
    tools: [write_file]          # this STEP narrows the agent's grant to one tool
    prompt: "Write your findings into report/summary.md."
    assert:
      files: [report/summary.md]   # it WROTE the file, not just claimed to
  - task: publish
    inputs: [report]
    run: cat report/summary.md
    assert:
      stdout: all clear            # ...and the artifact reached the next step
  assert:
    execution: [writer, publish]
    outcome: succeeded
```

A step's `tools:` **selects from** what the agent already grants — it can narrow, never widen. Naming a tool the agent does not provide is a load error, so one careless step cannot hand a model a capability the pipeline never gave it; an absent step `tools:` means the agent's whole grant, unchanged.

### `edit_file`

Takes `path`, `old_string`, `new_string`, and optional `replace_all`. `old_string` must match **exactly once** unless `replace_all` is set; zero matches and ambiguous matches are both returned as errors phrased as next-turn instructions, since both are recoverable without burning an attempt. The file's mode is preserved. Returns `replacements`, `first_line`, and `match_mode`, never content.

Matching is forgiving, in three strategies tried in order of decreasing exactness: **exact** first; then **line-trimmed** (every line matches modulo leading/trailing whitespace — recovers the classic local-model miss of right block, wrong indentation); then **block-anchor** (for a block of 3+ lines, the first and last lines anchor and the middle is judged by per-line similarity). The matched span is always the file's *own* text, so a forgiving match never rewrites untouched lines to the model's spelling. `match_mode` in the result says which strategy landed — an inexact edit is visible, not silent.

`edit_file` pairs with `read_file` by design: `read_file` returns **raw bytes**, so text copied out of one is a byte-exact `old_string`. Line numbers come from `search_files`' `content` mode instead — never from `read_file`.

### `search_files`

Supply `pattern` (a regexp matched against each line), `glob` (a shell pattern matched against a file's path), or both; `glob` alone is a filename search. `path` defaults to `"."`. Three `output_mode`s:

- `files_with_matches` (default) — just the paths, cheapest; read the ones you want with `read_file`.
- `content` — matching lines **with line numbers**, each capped at 500 bytes. This is where a persona gets `file:line` to cite.
- `count` — matches per file.

Unlike every other tool, `search_files` **never spills**: its bound is arithmetic — content matches accumulate against a 28KB budget, so a saturated result lands under the 32,000-byte inline cap by construction. `head_limit` caps results (default 50, ceiling 200); `total` and `truncated` report the true scale, so the answer to a flooded result is a narrower pattern, not a second page. `.git`, `node_modules`, `vendor`, binary files, and files over 2MB are skipped. `**` is supported only as a leading glob segment (`**/*.go`).

`write_file` requires the file's immediate parent directory to already exist — use `run_shell` (`mkdir -p`) first if it doesn't. Like every file tool, its path is confined to the working directory and re-validated against symlink escapes.

### `web_fetch`

Takes one argument, `url` (`http://` or `https://` only), and returns `{status, content_type, body, truncated}`. The body is capped at 100KB inline and cut off past it (`truncated: true`) — the overflow is not spilled, because the page is still on the web and a narrower URL fetches the rest.

A bare grant reaches any http(s) URL — the same trust level as `run_shell`, which can already `curl` anywhere. The mapping form's `allow:` is for the agent granted a browser but **not** a shell: each entry matches its exact hostname and any subdomain, case-insensitively, and every hop of a redirect chain is re-checked, so a permitted host that 302s elsewhere is refused mid-flight. The fence is enforced in steps, not in prompt language — a refused fetch comes back as `{"error": ...}` data naming the host and the list, which matters precisely because this tool exists to read pages the pipeline does not control:

```yaml test=agents-web-fetch
agents:
- name: auditor
  source: { model: openrouter/qwen/qwen3.7-flash }
  tools:
  - write_file
  - builtin: web_fetch
    allow: [specification.website]   # this host and its subdomains; empty/absent = any http(s) URL

jobs:
- name: audit
  plan:
  - agent: auditor
    outputs: [notes]
    prompt: "Check the tracker at issues.example and record what you find in notes/status.md."
    assert:
      tool_calls:
      - name: web_fetch
        args: { url: "https://issues.example/open" }   # outside allow: — refused as tool-result data
      files: [notes/status.md]
  - task: show
    inputs: [notes]
    run: cat notes/status.md
    assert:
      stdout: refused
  assert:
    execution: [auditor, show]
    outcome: succeeded
```

The refusal happens before any connection is attempted, so the example above runs without network — and the model, told about the fence in the tool's own description, records the refusal instead of retrying it.

Two rules keep a written fence honest, both enforced at load:

- **`allow:` belongs on the `agents:` entry that grants the tool.** A step's `tools:` *selects* from the grant, and a selection is resolved by substituting the agent's own spec — so an `allow:` written on a step would read as a fence and bind nothing. Select by bare name (`tools: [web_fetch]`) and the agent's fence comes with it.
- **Entries are bare hostnames** — no scheme, path, port, or wildcard. A host already covers its subdomains, and a pattern-shaped entry is refused rather than interpreted, because the two backends read the list differently enough that one written fence could otherwise mean two different things (see below).

## Working directory, inputs, and dir:

An agent step's `dir:` sets its working directory *and* names the artifact it operates in (its first path component — `dir: repo/cmd` names `repo`). That artifact must be one of the step's own declared `inputs:` (or `outputs:`), and it's flow-validated like any input — an agent pointed at a directory nothing fetched ("summarize the repository" with no `get`) fails at plan time, before the model is ever called. See [workspace.md](workspace.md).

```yaml test=agents-dir
agents:
- name: reader
  source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
- name: inspect
  plan:
  - task: fetch
    outputs: [repo]
    run: |
      mkdir -p repo/cmd
      echo 'package main' > repo/cmd/main.go
  - agent: reader
    inputs: [repo]
    dir: repo/cmd                 # start here; `repo` is the artifact it names
    prompt: "What package does main.go declare?"
    assert:
      tool_calls:
      - name: read_file
        args: { path: main.go }   # relative to dir:, not to the step's root
      stdout: package main
  assert:
    execution: [fetch, reader]
    outcome: succeeded
```

Every tool path the model uses is relative to `dir:`, which is the point: a model handed the subdirectory it is meant to work in does not spend turns navigating to it, and cannot wander out — the file tools stay confined to the step's workspace regardless.

## Custom tools, `required:`, and call guards

A custom tool is a `tools:` entry with `name`/`description`/`run` — a [templated](templating.md) shell command whose parameter schema is inferred from the `{{ .args.* }}` references in its `run:`. It can be marked `required: true`: the step can't complete until that tool has *succeeded*. It may also set `max_calls:` (a per-conversation budget) and `args:` (pinned values the model never sees):

```yaml test=agents-custom-tool
agents:
- name: reviewer
  source: { model: openrouter/qwen/qwen3.7-flash }
  tools:
  - read_file
  - name: post_review
    description: Post the review. body is the review text.
    run: |
      echo posting to {{ .args.repo | shellquote }} {{ .args.body | shellquote }}
    required: true         # the step fails unless this succeeds
    max_calls: 1           # at most once per conversation
    args:
      repo: jtarchie/ci    # pinned — the model neither sees nor can override it

jobs:
- name: review
  plan:
  - agent: reviewer
    prompt: "Review the change and post your conclusion."
    assert:
      tool_calls:
      - name: post_review
        args: { body: looks correct }   # only model-authored args are assertable —
                                        # naming the pinned `repo` here is a load error
  assert:
    execution: [reviewer]
    outcome: succeeded
```

- **No tool failure, required or not, ever aborts or restarts the conversation.** A failed call comes back to the model as ordinary data (`{exit_code, stdout, stderr}` or `{"error": ...}`), exactly like `run_shell`, so the model sees what went wrong and can recover in the same session.
- **`required:` tracks success** (`exit_code == 0`), not mere invocation. If the model tries to stop before a required tool has succeeded, its next turn is constrained — via the provider's `tool_choice` — to a call of that specific tool. A hard API-level constraint, not a text reminder.
  - Some OpenAI-compatible local servers (LM Studio confirmed) reject the named-object `tool_choice` form. `source.string_tool_choice:` picks which form is sent — unset, it defaults to the string form for a resolved `lmstudio`/`ollama` provider and the precise named form otherwise. The string fallback only guarantees *some* tool call, so `max_turns` is still the real backstop.
- **The safety bound is `max_turns`**: if a provider ignores `tool_choice`, or the model just can't get it right, the loop still terminates and the step fails, naming the tool(s) that never succeeded.
- **`args:` pinning**: a pinned key is excluded entirely from the schema the model sees. At execution, the pinned value is merged in *over* whatever the model supplied, before rendering and before the missing-argument check. The model chooses *when*, the pipeline chooses *where*.
- **`max_calls:` budget**: once exhausted, the next call is rejected before it reaches the tool's implementation (no side effect runs) and comes back as `{"error": "... call budget exhausted ..."}`. A budget-rejected call never counts toward `required:`.
- **Only custom tools can be `required:`**, and `max_calls:`/`args:` are rejected at load on builtin and sub-agent tools. A builtin can be written as a mapping (`{builtin: read_file}`) purely so those fields have somewhere to attach.
- **Quote everything that reaches the shell.** `{{ .args.body | shellquote }}` — an LLM-authored value with backticks in it is otherwise command-substituted. See [templating.md](templating.md).

## Delivering files the pipeline will read

An agent's answer is not its final message. Everything downstream — a `put:`, a task, another agent — reads **files**, and a model that summarizes its work in prose instead of writing it has produced nothing while sounding finished. `assert.files:` already states which files a step owes ([control-flow.md](control-flow.md#assert-self-verification--steps-test)); on an agent step it is also enforced *while the model can still act on it*:

```yaml test=agents-delivers-files
agents:
- name: responder
  source: { model: openrouter/qwen/qwen3.7-flash }
  tools: [write_file]

jobs:
- name: answer
  plan:
  - agent: responder
    outputs: [answer]
    prompt: "Answer the question. Write your answer to answer/reply.md."
    assert:
      files: [answer/reply.md]
  - task: deliver
    inputs: [answer]
    run: cat answer/reply.md
    assert:
      stdout: widgets.json
  assert:
    execution: [responder, deliver]
    outcome: succeeded
```

The model in that example answers in prose first and writes nothing. Rather than ending the step there, the conversation puts it back:

> You are trying to finish, but this step declared files it must leave behind and they are not there: `answer/reply.md` does not exist. Your final message is not the deliverable — a later step of this pipeline reads these files, and text you write in this conversation reaches nobody. Write them now using the tools you have, then finish.

- **It fires when the model tries to stop**, not after the step is over — the same moment `required:` tools are forced, for the same reason: afterwards there is no one left to tell.
- **Five chances, and they do not reset.** A model that keeps answering in prose runs out and the step fails naming the file, which is what an unmet `assert.files` always did. The nudge can only give the model chances to change the outcome; it never changes the outcome itself.
- **Every missing file is named at once.** Told one at a time, a model pays a turn per artifact to learn what one sentence could have said.
- **An empty file is a missing file** — the same rule the assert already used, which matters here because `touch`ing the path is what a model reaching for the letter of the instruction does.
- **It gates `verdicts:`.** A step declaring both cannot record a decision while its files are absent: the `verdict` tool refuses with an error naming them. A verdict is the model's report on the work and the files *are* the work, so a decision accepted over missing artifacts is precisely the false success worth preventing — the report routes the job, and the job goes green having produced nothing.
- **CLI-backed agents get the same thing through a resumed session.** A subprocess owns its own tool loop, so the check runs after it exits and the child is woken with `--resume` and told what is missing — its conversation, its working directory and its task all still intact. A restart would re-send work it has already done; see [attempts-timeout.md](attempts-timeout.md#what-attempts-costs-on-an-agent). A child woken because it *died* still gets the "continue, do not start over" prompt it always did, with the missing files added; the two reasons for rejoining are not interchangeable. The nudge and `attempts:` share one budget of `attempts + 5` child invocations for the step, rather than each round getting a fresh `attempts:` — otherwise the two limits multiply.
- **When the step gives up, the failure names the file**, whichever backstop caught it — the refused verdict, a forced tool, the loop detector, the turn ceiling. All of them are downstream of the same cause, and naming the mechanism instead would point an operator at the symptom. An infrastructure error is left alone: the missing file is incidental to a transport failure, and restating it would turn an errored step into a failed one and fire the wrong hook.

Nothing to enable and no field to set. `assert.files:` was already the contract; the only question was whether the one party who could still honor it got to hear about it.

## Sub-agent delegation (`agent:` tools)

A `tools:` entry can be a sub-agent tool — `{ agent: <name>, description: <text> }` — exposing another `agents:` entry to the parent model as a callable tool taking a single `request` string. "Delegate and get an answer back":

```yaml test=agents-subagent
agents:
- name: summarizer
  source: { model: openrouter/qwen/qwen3.7-flash }
  description: Condenses a file to a paragraph.   # what a PARENT sees by default
  tools: [read_file]
- name: lead
  source: { model: openrouter/qwen/qwen3.7-flash }
  tools:
  - read_file
  - agent: summarizer            # a sub-agent, exposed as a callable tool
    description: Summarize a file; pass the path in `request`.

jobs:
- name: digest
  plan:
  - task: fetch
    outputs: [notes]
    run: echo 'a very long account of the outage' > notes/log.txt
  - agent: lead
    inputs: [notes]
    prompt: "Have your summarizer condense notes/log.txt, then report."
    assert:
      tool_calls:
      - name: summarizer         # the child was reached as an ordinary tool call
      stdout: there was an outage
  assert:
    execution: [fetch, lead]     # the child records nothing of its own
    outcome: succeeded
```

- Each call runs the child's own fresh conversation (its own model, persona, dials, `max_turns`, tool grant) and returns its final text as the tool result, capped at 32,000 bytes like any other tool output.
- The child runs in the **caller's** working directory but under the **child's own** resolved image — a sub-agent is a different worker, unlike a fix agent (which reproduces the failing task's image).
- The child's LLM client and tool tree are built eagerly during the parent step's preparation, so a bad credential fails before the first call, not during it.
- No cache node, `job_run`, or execution-log entry is recorded for the child conversation — same no-record contract as `fix:` agents and hook steps.
- A child failure comes back to the parent as `{"error": ...}`, never a Go error that aborts the parent conversation.
- A sub-agent is a capability grant like `run_shell`: a step selects a granted one by bare name and cannot introduce one inline.
- **Load-time graph checks**: a sub-agent tool must set no `builtin`/`name`/`run`, can never be `required:`, and must reference an existing agent. The agent graph is walked for cycles and capped at a nesting depth of 8. A fix agent may not grant sub-agents.
- **Caching**: a sub-agent tool folds in the child's resolved invocation content (model/endpoint/persona/dials/max_turns/image + its own tools, recursively), so editing a child — or grandchild — busts the parent step's hash.

## Self-healing tasks (`fix:`)

`fix:` attaches an agent to a **task** step: if the command exits non-zero, the agent is invoked to repair whatever broke, and then the command runs again. A green run never constructs the agent at all. This example is real: the task fails, the fixer creates the missing file, the re-run passes:

```yaml test=agents-fix
agents:
- name: fixer
  source: { model: openrouter/qwen/qwen3.7-flash }
  system: You fix failing checks. Make the smallest change that works.
  tools: [read_file, search_files, edit_file, write_file, run_shell]

jobs:
- name: build
  plan:
  - task: check
    run: test -f config.json
    fix: fixer                 # scalar: just the agent name
  assert:
    execution: [fixer, check]  # the fixer ran and the re-run passed; a green first
    outcome: succeeded         # try would record [check] alone
```

The mapping form takes per-task overrides:

```yaml fragment
    fix:
      agent: fixer
      prompt: Only fix compile errors; never touch a test assertion.
      dir: repo
      tools: [read_file, edit_file]   # narrow the agent's grant for this task
      attempts: 2
      timeout: 10m
```

**How the loop terminates.** The agent is seeded with the failing command's captured output and given the parent task itself as a zero-arg **rerun tool** (its `run:`, never its `fix:`, so a rerun cannot recurse). It can edit, rerun, and see the new output. When the conversation ends, `steps` runs the command one final time, and *that* exit code decides the step. There is no repeat-until-green loop: one agent conversation, then one verdict. A still-red command fails the step normally, firing `on_failure`.

**What a fix agent needs.** It must be able to edit, and the default tool grant deliberately excludes the write tools — grant them explicitly.

**Restrictions.** A fix agent may not grant sub-agents, may not use MCP tool grants, and may not set `image:` (it runs in the parent task's image). Its conversation records no cache node or `job_run` of its own.

**Caching.** A task with a `fix:` makes its chain uncacheable, since whether it succeeds may depend on what a model did. The run prints `note: <step> makes this chain uncacheable (fix: agent)` at the point that happens.

## Top-level `tasks:` reuse

A top-level `tasks:` list (mirroring `resources:`/`agents:`) lets a `run:`/`fix:` pair be defined once and reused across jobs. A job's `task:` step is disambiguated by whether it carries its own `run:`:

```yaml test=agents-tasks-reuse
agents:
- name: fixer
  source: { model: openrouter/qwen/qwen3.7-flash }
  tools: [read_file, write_file, run_shell]

tasks:
- name: unit
  run: echo running unit tests
  fix: fixer                 # the run:/fix: PAIR, defined once, reused by both jobs

jobs:
- name: quick
  plan:
  - task: unit               # run: absent -> resolves the tasks: entry
    assert:
      stdout: running unit tests
  assert:
    execution: [unit]
    outcome: succeeded
- name: full
  plan:
  - task: unit
    assert:
      stdout: running unit tests    # the same tasks: entry, resolved again
  - task: integration        # run: present -> inline; tasks: never consulted
    run: echo running integration tests
    assert:
      stdout: running integration tests
  assert:
    execution: [unit, integration]
    outcome: succeeded

assert:
  execution: [quick, full]   # pipeline level: the jobs `steps test` must have run
```

Neither execution assert names `fixer`, and that is the assertion: a green command never constructs its fix agent. Had either command failed, the `fix:` these steps inherit would have run first and the step `assert:` would then have judged the re-run — a reused definition brings its repair behavior with it. The repair path itself is exercised in [attempts-timeout.md](attempts-timeout.md); what is reused here is the definition.

This resolution runs identically at plan time and run time, so a task's cache hash is always computed from its *resolved* `run:` string. An undefined reference is an ordinary error at plan time. An agent step's connection/dials/tool-grant resolve the same way.

## External files: `run_file:`, `system_file:`, `prompt_file:`, and `file:`

A task's `run:`, an agent's `system:` persona, an agent step's `prompt:`, and a `fix:`'s `prompt:` can all be loaded from a file instead of written inline — useful since a persona is often long freeform prose, and a `run:` a full shell program:

```yaml test=agents-files
tasks:
- name: unit
  run_file: ci/unit.sh              # loads the task's run: from a file

agents:
- name: reviewer
  source: { model: openrouter/qwen/qwen3.7-flash }
  system_file: prompts/reviewer.md  # loads the agent's persona from a file

jobs:
- name: build
  plan:
  - task: unit
    assert:
      stdout: unit tests pass       # ci/unit.sh's text, resolved at load
  - task: smoke
    run_file: ci/smoke.sh           # a plan step's OWN run:, from a file
    assert:
      stdout: smoke ok
  - agent: reviewer
    prompt_file: prompts/review.md  # loads the step's prompt from a file
    assert:
      stdout: Build looks fine
  assert:
    execution: [unit, smoke, reviewer]
    outcome: succeeded
```

Every `*_file:` path is resolved **once, at load time**, relative to the pipeline YAML's own directory — so everything downstream sees the resolved text and cannot tell it apart from the same value written inline. Editing an included file busts the cache exactly like editing an inline value would.

A path may use `..` to escape the pipeline's own directory: the pipeline file is trusted input, and a file placed beside it by the same author is at the same trust level — a shared `../tasks/` directory is a legitimate layout. Setting both a field and its `*_file:` sibling is a load-time error, and so is an empty included file.

A top-level `tasks:`/`agents:` entry additionally accepts a whole-document `file:`, loading a complete `Task`/`Agent` definition from a separate YAML file so it can be shared across pipelines:

```yaml test=agents-task-file
tasks:
- name: unit
  file: ci/unit.yml         # supplies run/fix/image/timeout/inputs/outputs
  timeout: 5m               # any field set here overrides the document's

agents:
- name: reviewer
  file: ci/reviewer.yml     # the same for a whole agent definition
  max_turns: 10             # ...and the same override rule

jobs:
- name: build
  plan:
  - task: unit
    assert:
      stdout: from the shared task   # the run: came out of ci/unit.yml
  - agent: reviewer
    prompt: "Review the build."
    assert:
      stdout: Nothing to flag
  assert:
    execution: [unit, reviewer]
    outcome: succeeded
```

The entry's own inline fields win over the loaded document's, and the loaded document may not itself use `file:`/`run_file:` — includes are resolved one level deep only, which is what makes cycle detection unnecessary.

### The run-time form: an agent step's `prompt_file:` from a fetched artifact

An agent step's `prompt_file:` additionally accepts a `{artifact, path}` mapping, naming a file inside an artifact a `get` step fetched, read at **run time** rather than load time:

```yaml test=agents-prompt-artifact
resource_types:
- name: repos
  config:
    check: |
      printf '[{"ref": "abc123"}]'
    in: |
      mkdir -p .ci
      echo 'Review this change for correctness.' > .ci/REVIEW.md

resources:
- name: repo
  type: repos
  source: {}

agents:
- name: reviewer
  source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
- name: review
  plan:
  - get: repo
    trigger: true
  - agent: reviewer
    inputs: [repo]
    prompt_file: { artifact: repo, path: .ci/REVIEW.md }
    assert:
      stdout: The change is correct
  assert:
    execution: [repo, reviewer]
    outcome: succeeded
```

This is the one place a step's config can come from a fetched artifact, and it is deliberately narrow — a task's `run:` and a whole agent definition cannot, for two reasons:

- **A task's `run:` already reaches into a fetched artifact today.** `run: sh repo/ci/build.sh` works with `inputs: [repo]` declared.
- **An agent's connection is a credential boundary a fetched repo must never cross.** `source.endpoint:`/`api_key_env:` decide where a configured API key gets sent; letting a repo supply either would let it redirect that credential to an attacker-chosen server. A prompt is just the task text the model already reads the repo to act on.

The artifact named must be declared in the step's own `inputs:` (checked at load), and the read is confined with the same symlink-aware guard the file tools use. It cannot be resolved at load time — the file doesn't exist until the `get` runs — which costs nothing, since an agent step's chain is already unskippable.

## `context_paths:` — files delivered as synthetic `read_file` results

An `agent` step can declare `context_paths:` — files whose contents are injected at conversation start as synthetic `read_file` tool results. The model sees the file contents as if it had called `read_file` itself, without consuming a turn:

```yaml test=agents-context-paths
agents:
- name: coder
  source: { model: openrouter/qwen/qwen3.7-flash }
  max_context_bytes: 100000    # what every step of this agent gets by default

jobs:
- name: build
  plan:
  - task: conventions
    outputs: [repo]
    run: echo 'always run go vet before committing' > repo/CONVENTIONS.md
  - agent: coder
    inputs: [repo]
    context_paths: [repo/CONVENTIONS.md]
    max_context_bytes: 400000  # ...except this step, which is handed more
    prompt: "State this project's convention in one line."
    assert:
      stdout: go vet           # answered from the injected file, no read_file turn spent
  assert:
    execution: [conventions, coder]
    outcome: succeeded
```

The point is not convenience but **guarantee**: conventions every invocation must follow are present from the first turn, instead of costing a `read_file` round trip the model might not bother with.

Paths are relative to the step's working directory and confined to its workspace, so in practice the file lives inside a declared input. They are read at **run time** (per attempt), which is what distinguishes them from `system_file:`: the persona is the pipeline author's own text, resolved once at load; `context_paths:` is content that arrives with a fetched artifact and can change between runs. A missing or escaping file fails the step at preparation, before a token is spent. A file that is merely too big (over `max_context_bytes:`, default 100KB — `0` lifts the ceiling entirely) is **truncated** instead, with a note pointing at `read_file`'s paging — the author writes a path, not a size, and `pr/pr.diff` is a correct path that would otherwise start failing the day the pull request grew.

`context_paths:` is a step-level field, not agent-level — the agent definition has no notion of which inputs are available. It requires `read_file` in the tool grant (which it is by default). Sub-agents and fix agents do not inherit the parent step's `context_paths:`. `max_context_bytes:` is spelled on **either**, and the step's wins (as above) — two steps sharing one agent routinely hand it different evidence. `context_window:` deliberately has no step spelling for the mirror-image reason: it describes the *model*, and the model belongs to the agent.

**In an `across:` matrix**, each entry renders `{{ .vars.<name> }}` per cell, so a cell arrives already holding the code it was assigned instead of spending its first turns navigating to it:

```yaml test=agents-across-context
agents:
- name: reviewer
  source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
- name: review
  plan:
  - task: fetch
    outputs: [repo]
    run: |
      echo 'package api'     > repo/api.go
      echo 'package storage' > repo/storage.go
  - across:
    - var: dim
      values: [api, storage]
    agent: reviewer
    inputs: [repo]
    context_paths: ["repo/{{ .vars.dim }}.go"]
    prompt: "Review the {{ .vars.dim }} package."
  assert:
    execution:                 # one cell per value, each under its own coordinates
    - fetch
    - reviewer [dim=api]
    - reviewer [dim=storage]
    outcome: succeeded
```

One path per entry, rendered per cell. A `{{ .vars.x }}` naming an axis the matrix does not declare is a **load** error naming the entry (`context_paths[0]`).

**Caching**: the *paths* (not contents) enter the step's hashed content — the files live inside the workspace, so their content is already chained through the input artifacts' own hashes. A matrix cell hashes the path it rendered to, which is what makes two cells reviewing different files two different steps.

## Reading another step's decision (`context: { from: ... }`)

A verdict is the one thing every judging step produces. A classifier that simply falls through, or a shell command that wants to branch on what a model decided, needs a way to ask for it. `from:` is that ask, and it is declared on the **reader**:

```yaml test=agents-context-from
agents:
- name: reviewer
  source: { model: openrouter/qwen/qwen3.7-flash }
- name: editor
  source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
- name: revise
  plan:
  - agent: reviewer
    prompt: "Review the change."
    verdicts: [approve, revise]      # no routing — this one just decides
    assert:
      verdict: approve               # what it decided, pinned at the source
  - agent: editor
    prompt: "Apply the review."
    context:
      from:
        reviewer: note               # verdict | note | full
  - task: gate
    context:
      from:
        reviewer: verdict
    run: grep -q 'approve' upstream/reviewer
    assert:
      code: 0                        # the verdict file really landed at upstream/reviewer
  assert:
    execution: [reviewer, editor, gate]
    outcome: succeeded
```

- **Levels.** `verdict` is the name it chose; `note` adds the reason it gave; `full` adds its final response text.
- **The demand creates the obligation.** Asking for a `verdict` costs the sender nothing — a step declaring `verdicts:` already must emit one. Asking for a `note` or `full` makes that sender's note **required**: it joins the verdict tool's required arguments, so the model cannot satisfy the call without writing one. A note nobody demanded is one a model may reasonably skip, and afterwards "chose not to" is indistinguishable from "forgot".
- **Nothing arrives unasked.** No `from:`, no delivery. An agent reader receives each decision as a synthetic `read_step` result at turn zero (like `context_paths:`, no turn spent); a task reader receives a file per sender at `upstream/<step>`, since a shell command has no conversation.
- **A sender that has not run yet is simply absent** — no error, nothing delivered. That is what makes a revise loop work: the writer at the top of the loop reads the critic *below* it, gets nothing on the first pass, and gets the verdict that sent it back on every pass after. Naming a step that comes *later* in the plan is legal — that is the loop.
- **Validated at load**: the named step must exist in the job and must declare `verdicts:`, and a step may not read itself.
- **Trust**: a delivered note or response is upstream model-authored text, so it is fenced as data with a tag that cannot occur inside it.
- **Caching**: the `from:` declaration folds into the reading step's hash, and makes a *task* reader's chain unskippable — a cached command never runs, so a task whose `from:` changed must not replay an outcome produced without the decision it now asks for.

## Model dials, and pipeline-wide `defaults:`

An `agents:` entry carries the sampling dials for its model, and `defaults:` supplies what every agent that names nothing gets:

```yaml test=agents-dials
defaults:
  model: openrouter/qwen/qwen3.7-flash   # any agent whose source: names no model
  delegate_budget_percent: 10            # the share a sub-agent takes of what is left
  preflight:
    timeout: 30s                         # per pre-run probe
    cache: 5m                            # a target verified this recently is trusted

agents:
- name: drafter
  source: { model: openrouter/qwen/qwen3.7-flash }
  temperature: 0.2            # dials, all optional — unset means the provider's own
  top_p: 0.9
  max_tokens: 2048
  reasoning_effort: low       # low | medium | high, for models that take one
  delegate_budget_percent: 25 # this agent's helpers get a bigger share
  preflight: false            # ...and this one skips the probe (a slow local model)
- name: titler                # no source: at all — defaults.model is its model

jobs:
- name: draft
  plan:
  - agent: drafter
    prompt: "Draft the release note."
    assert:
      stdout: Drafted
  - agent: titler
    prompt: "Title the release note."
    assert:
      stdout: Titled
  assert:
    execution: [drafter, titler]
    outcome: succeeded
```

- **A dial left unset is not sent**, so the provider's own default applies — steps never invents a temperature. `reasoning_effort:` is passed through to models that accept one and ignored by those that don't; a value outside `low`/`medium`/`high` is a load error.
- **Every dial folds into the step's hash**, unlike the operational limits (`attempts:`/`timeout:`/`budget:`): changing how the model samples changes what the step produces, so a cached result from one setting must not stand in for another.
- **`defaults:` is a fallback, never an override.** Anything an agent states for itself wins; `defaults.model` fills in only for an agent whose `source:` names no model, which is what lets a whole pipeline be pointed at a different model by editing one line.
- **`preflight:` composes both ways** — tune it pipeline-wide under `defaults:`, opt one agent out with `preflight: false`. See [the preflight section](README.md#commands).

## Budgets: `budget.tokens`

An agent step can loop, hold a long conversation where every turn re-sends the whole history, and retry. `budget:` is the ceiling on that — the AI equivalent of `timeout:`:

```yaml test=agents-budget
agents:
- name: writer
  source: { model: openrouter/qwen/qwen3.7-flash }
  budget:
    tokens: 200000      # per invocation of this agent

jobs:
- name: publish
  budget:
    tokens: 500000      # cumulative, across every agent step in the job
  plan:
  - agent: writer
    prompt: "Write the release announcement."
    assert:
      stdout: Announcement written
  assert:
    execution: [writer]
    outcome: succeeded
```

An agent's ceiling covers **the agent and everything it delegates to**. A sub-agent draws on its parent's remaining allowance rather than adding to it, so `budget.tokens` bounds the whole delegation subtree instead of one conversation in it — otherwise a capped agent could delegate its way past its own ceiling without ever exceeding it.

- **Each call takes a share of what's LEFT**, 10% by default. A fraction of the remainder rather than of the original means delegation can never drain a parent outright: successive helpers take a tenth of a shrinking number, and the parent keeps something to finish its own work with.
- **The tighter number wins.** A sub-agent that declares its own `budget.tokens` gets the smaller of that and the parent's share, so neither an inherited allowance nor a declared one can be exceeded.
- **A parent with no ceiling changes nothing** — there is no allowance to take a fraction of, so its children run on their own declared budgets exactly as before.
- **A delegation the parent cannot fund is refused**, and the model is told so as an ordinary tool result. It can then finish without the helper rather than the step dying.
- **The share is tunable** with `delegate_budget_percent:`, pipeline-wide under `defaults:` or per agent:

```yaml fragment
defaults:
  delegate_budget_percent: 10   # the default; every agent unless it says otherwise

agents:
- name: lead
  budget:
    tokens: 400000              # bounds `lead` AND every helper it calls
  delegate_budget_percent: 25   # this one's helpers do the heavy lifting
  tools: [{ agent: researcher }]
```

A run that resumes continues its **job** budget from what earlier attempts already spent, rather than starting the allowance over — otherwise `budget:` would be a per-attempt ceiling wearing the name of a per-run one, and every resume would buy another full one.

**Reporting happens whether or not you set one**, which is the point: it is what tells you which ceilings are even sensible. Every job that ran an agent step prints what it cost — **and records it**, so the question survives the terminal:

```
$ steps runs pipeline.yml --cost
RUN                 TOKENS   CACHED        COST   STEPS
r-8f2a1c         4,102,338      38%    unpriced       9

$ steps runs pipeline.yml --cost --run r-8f2a1c
STEP                                TOKENS   CACHED   DURATION  FINISH
reviewer [dim=state-mutation]      412,880      61%       1m02s  stop
reviewer [dim=api]               1,204,551      22%      14m30s  length  <-- truncated
```

- **CACHED** is the only place prompt caching reports whether it worked — without it the feature is faith-based.
- **FINISH** distinguishes a model that had little to say from one **cut off** by its output limit; a truncated verdict wastes every step downstream and otherwise reads as an ordinary short answer.
- **COST** says `unpriced` rather than `$0.00` — a zero would say the run was free instead of that nobody priced it.

Things worth knowing:

- **The numbers are the provider's own reported usage**, never an estimate. A provider that reports no usage contributes nothing — a ceiling must never trip on a number nobody reported.
- **A breach stops the step before its next tool calls run**, so a step that has blown its ceiling does not go on to have side effects.
- **A job breach reports the running total per step**, because a cumulative ceiling is rarely tripped by the step that cost the most.
- **A breach classifies as `errored`, not `failed`** — an operational limit being hit, not the model producing a bad answer. `on_error` fires; no `to:` route can treat it as a decision the model made.
- **Never hashed.** Adding a budget after reading a usage report must not invalidate every cached step.
- **A sub-agent has its own budget**, from its own `agents:` entry; its spend rolls into the job total.
- **Tokens only, deliberately.** A money ceiling would need a per-model price table that goes stale every time any provider changes its rates. (CLI agents are the exception — see below.)

## Failover: `fallback:`

When an agent's model is unreachable, try a backup instead of retrying a dead connection:

```yaml test=agents-fallback
agents:
- name: writer
  source:
    model: openrouter/qwen/qwen3.7-flash
  fallback:
  - source:
      endpoint: https://backup-provider.example.com/v1/
      model: equivalent-model
      api_key_env: BACKUP_KEY

jobs:
- name: publish
  plan:
  - agent: writer
    prompt: "Write the announcement."
    assert:
      stdout: Announcement written   # the primary answered, so no source changed
  assert:
    execution: [writer]
    outcome: succeeded
```

`fallback:` fires two ways, automatically — declaring it is what opts an agent into both, there's no separate switch for the second:

- **Before the run, from the pre-run probe.** A primary that fails preflight is exactly when to pick an alternate: before anything has been spent. Sources are tried in order; the first that answers serves the whole run.
- **Mid-run, when `attempts:` gives up on the source actually running.** A primary that passes preflight and then starts erroring mid-conversation exhausts its `attempts:` budget the same as any agent step — but instead of just failing, the step moves to the next `fallback:` source and **resumes the same conversation** there rather than asking the task over: whatever the dead source already did (turns taken, tools called, results returned) carries over as the resumed request's history. Each source in the cascade gets its own `attempts:` budget in turn — a primary and two fallbacks can spend up to three sources' worth of `attempts:` before the step actually fails.

```yaml test=agents-fallback-midrun
agents:
- name: writer
  source:
    model: openrouter/qwen/qwen3.7-flash
  fallback:
  - source:
      endpoint: https://backup-provider.example.com/v1/
      model: equivalent-model
      api_key_env: BACKUP_KEY

jobs:
- name: publish
  plan:
  - agent: writer
    attempts: 1   # no room to retry — the very first failure trips the cascade
    prompt: "Write the announcement."
    assert:
      stdout: Announcement written via the fallback   # this time the fallback actually served the run
  assert:
    execution: [writer]
    outcome: succeeded
```

- **Connection-level failures only** — an unreachable endpoint, a 5xx, a connection that died mid-response. A model *refusing* a request is a different class entirely; falling over on one would silently reroute a legitimate refusal to a possibly less suitable model. A conversation that ran out of turns, or that a model refused, is never eligible either way — only the mid-run trigger has a request to inspect for this, but the rule is the same one preflight's trigger already implies.
- **Resume only between two hosted sources.** A CLI-backed source (`@claude/sonnet`-style) already has its own resume mechanism — a session it rejoins — and its own dollar-metered budget, so it sits outside this cascade entirely: a CLI primary's own failure doesn't trigger it, and a hosted cascade that reaches a CLI `fallback:` entry stops there rather than skipping past it. `fallback:` still reaches a CLI source exactly as before, via the pre-run probe.
- **Only the source changes.** Persona, dials, limits and tool grant are untouched: an outage changes where requests go, never what the agent is allowed to do. The compaction budget does follow whichever model actually serves the run.
- **Never hashed.** Which source served a run is *availability*, not content — the alternative would invalidate every agent step at exactly the moment things are already going badly.
- **Loudly visible.** A run that used a fallback says so in the log (`agent.failover`), on the step's own output line, and in the recorded result (`fallback_model`).
- **Every fallback endpoint is validated** like the primary — no credentials in the URL, and the provider prefix must resolve at load.
- **Pinned for the process, either way.** Once a source (preflight-picked or mid-run-picked) has *served* one run, a `steps watch` process keeps using it rather than re-probing or re-failing-over on every poll.

  Both directions take positive evidence, because nothing re-examines a pin later — the pre-run probe only ever checks the *primary*. A source is pinned only after it carried a conversation to an end, which a provider answering `400` did not do. A pin is dropped only after the cascade actually *tried* the alternatives and none of them served; a step that failed without ever swapping has learned nothing, and dropping the pin there would send the next step back to a primary the probe may already have found dead. Anything ambiguous — the step spending its own `timeout:`, the run being cancelled — changes nothing in either direction.
- **One deadline and one turn budget for the whole cascade.** `timeout:` and `max_turns:` bound the STEP, not each source: a resumed conversation continues both counts rather than restarting them, so three `fallback:` entries under `timeout: 10m`/`max_turns: 30` still cost at most ten minutes and thirty turns. See [attempts-timeout.md](attempts-timeout.md).

  A step that spends that whole deadline therefore ends there rather than cascading. Not because a hung endpoint is not worth escaping — it is the most expensive outage shape there is — but because the deadline is shared: every remaining source would start already expired, turning one outage into as many failures as the list is long.
- **Sub-agents follow the pre-run probe only.** A granted sub-agent runs on whichever source preflight selected for it, but has no mid-run cascade of its own — a delegation is one conversation, and its failure returns to the parent as tool-result data for the parent to react to.

## CLI-backed agents: `@claude/sonnet`

An agent's `source.model` normally names a hosted model steps calls over HTTP. Prefix it with `@` instead and steps runs a coding-agent CLI as a subprocess:

```yaml noexec=cli
agents:
- name: reviewer
  source:
    model: "@claude/sonnet"     # quotes required -- YAML reserves a leading @
  tools: [read_file, run_shell]
  settings: project             # opt in to the repo's checked-in .claude/ scope
  budget:
    usd: 0.50                   # CLI agents meter in dollars, not tokens

jobs:
- name: review
  plan:
  - task: fetch
    outputs: [repo]
    run: echo 'package main' > repo/main.go
  - agent: reviewer
    inputs: [repo]
    dir: repo
    prompt: "Review this code."
```

The quotes are not stylistic: a leading `@` is a reserved indicator in YAML, so an unquoted value is a parse error before steps ever sees it. `@claude/sonnet` reads as "the claude CLI, asked for sonnet" — the part after the slash is passed through untouched.

### What changes, and what doesn't

This is **delegation, not a different transport**. The CLI owns the conversation: its own turn loop, its own tools, its own context window. steps owns everything around it, unchanged — the workspace, the merkle hash that decides whether the step runs at all, `timeout:`, the recorded trajectory and response, `assert:`, and `verdicts:`/`to:` routing. steps also reads the CLI's transcript as it streams, so the turns it takes are published and stored exactly as a hosted agent's are — a delegated step is not a quieter one. You get the CLI's own tooling inside a pipeline that still caches, routes, and fans out.

**Authentication** comes from the CLI's own credential store — the subprocess inherits `HOME`, so a subscription login works with no `api_key_env:` at all. Set `api_key_env:` only to forward a specific key as `ANTHROPIC_API_KEY`.

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
| `web_fetch` | `WebFetch` |

One row diverges in contract, deliberately: the CLI's `WebFetch` takes a URL *and a prompt* and answers with a model-written summary of the page, where the hosted path's `web_fetch` returns the raw body.

An `allow:` list binds on both paths, but it is *compiled* for this one: each entry becomes a **pair** of permission rules, `WebFetch(domain:host)` and `WebFetch(domain:*.host)`, because the CLI matches domains exactly where steps matches a host and its subdomains — one rule alone would deny the subdomains, the other would deny the apex. This is also why a wildcard entry is a load error rather than a pattern: `*` denies everything here and matches everything there. One divergence is left standing, since it belongs to the CLI's engine: it checks the domain of the request, not each hop of a redirect chain.

Everything else in the grant — custom `run:` tools, `mcp:` grants, and the synthesized `verdict` tool — reaches the CLI over a loopback MCP server steps starts for the step and tears down after. Those are the *same* implementations a hosted agent runs. Credentials stay in the parent process; nothing reaches the CLI's config but a URL and a single-use token.

Anything not granted is **absent**, not merely unapproved: the grant becomes the CLI's entire built-in surface. That is deny-by-default — a capability this build of steps has never heard of is withheld because it was never granted, rather than surviving because nobody remembered to forbid it. The CLI's own configured MCP servers are excluded too.

### A step is not your session

A CLI agent step runs with **no configuration scopes by default**. Your personal `~/.claude` never applies — no user settings, hooks, plugins, skills — and the repo's own `.claude/` scope loads only when the agent opts in with `settings: project` (as above). A pipeline whose behavior depends on who ran it is not a pipeline. The opt-in is hashed, so granting or revoking it invalidates the step's cache. It is also markedly cheaper: dropping user-level config cut a trivial one-step pipeline from ~76K prompt tokens to ~25K in a measured run.

### Verdicts are enforced at exit

A hosted agent that tries to finish without its required verdict gets forced into one more call via `tool_choice`. There is no such lever across a process boundary, so the rule moves to the exit: **a step that declared `verdicts:` and finished without calling the verdict tool has failed.** The failure is routable, so a `failure:` entry catches it. The verdict itself is captured in the parent process the moment the tool is called, over the bridge — the CLI is never trusted to report what it decided.

### `attempts:` resumes the conversation

On the hosted path `attempts:` retries one HTTP request underneath a conversation that survives. A CLI agent gets the same guarantee by a different mechanism: the step names a session up front, and every retry **rejoins** it rather than starting the task over. The retried process is told what went wrong and to continue. Only *infrastructure* failures are retried — the process failed to start, exited nonzero, or died without reporting a result. A CLI that ran fine and concluded the task failed is an answer, not an outage.

- **The turn budget is per step, not per attempt** — `max_turns` counts across the whole conversation.
- **The transcript is cleaned up** — steps deletes the step's own session file afterwards.

### What a CLI agent cannot do

These are load errors, not silent no-ops, because a setting that reads as configured while binding nothing is worse than one that is rejected:

| rejected | why |
| --- | --- |
| `source.endpoint:` | there is no request to aim anywhere |
| `temperature:`, `top_p:`, `max_tokens:`, `reasoning_effort:` | the CLI chooses its own sampling |
| `source.string_tool_choice:` | no `tool_choice` on the wire to spell |
| `compact_after_tokens:`, `context_window:` | the CLI compacts its own conversation |
| `budget.tokens:` | nothing counts tokens until the subprocess exits (use `budget.usd:`) |
| `required:`, `max_calls:`, `args:` on a tool | enforced by the turn loop the CLI replaces |
| sub-agent tools, in either direction | a sub-agent nests inside a turn loop there is none of |
| a CLI agent as a task's `fix:` agent | same reason |
| `network: none` together with `image:` | the CLI reaches its steps-provided tools over a connection back to this process |

### Containerizing a CLI agent

`image:` **is** supported, and it does more here than for a hosted agent: it containerizes the CLI process itself, so its native tools are confined to the container rather than running on the host with only the working directory as a fence. Credentials are the part that needs a decision — a Linux subscription login is bind-mounted in read-only; macOS keeps it in the Keychain where no container can reach it, so `source.api_key_env:` is the portable answer. See [infra.md](infra.md#cli-agents).

### Budgets are in dollars

A CLI agent takes `budget: {usd: 0.50}` rather than `budget: {tokens:}`. The two runners meter different things and neither converts into the other honestly — each takes the unit it can enforce, and the other spelling is a load error. A job-level `budget:` stays in tokens and still counts what a CLI agent spent (reported on exit, folded into the job total).

`fallback:` works in both directions — a CLI agent can fall back to a hosted provider, and a hosted agent to a CLI. Preflight checks a CLI target by looking for its binary on `PATH` (or, containerized, by probing the image).

## Ensembles: asking several agents the same question

A single model has blind spots. Ask one reviewer "is this correct?" and you get one opinion with no signal about how much to trust it; ask three and require a majority, and one model's bad day stops being decisive:

```yaml test=agents-ensemble
agents:
- name: reviewer-a
  source: { model: openrouter/qwen/qwen3.7-flash }
- name: reviewer-b
  source: { model: openrouter/qwen/qwen3.7-flash }
- name: reviewer-c
  source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
- name: gate
  plan:
  - ensemble:
      verdicts:                       # the vocabulary EVERY member votes in,
        - reject: revise              # and where the BLOCK's decision goes
        - approve: publish
      decide: majority                # or: unanimous, any, or an agent name
      member_errors: fail             # or: exclude
      agents:
      - {agent: reviewer-a, prompt: "Review the diff for correctness."}
      - {agent: reviewer-b, prompt: "Review the diff for style."}
      - {agent: reviewer-c, prompt: "Review the diff for security."}
  - task: revise
    run: echo sending back
  - task: publish
    run: echo shipping
    assert:
      stdout: shipping
  assert:
    execution:                        # every member voted, then the majority's
    - reviewer-a                      # target ran — revise is absent, which is
    - reviewer-b                      # what "routed past it" looks like
    - reviewer-c
    - publish
    outcome: succeeded
```

### ⚠️ N agents cost N times one

Three reviewers cost three reviews, every run. This is the step where a job-level `budget:` earns its keep.

### The decision rules

- **`majority`** — the verdict more than half the voters chose.
- **`unanimous`** — every voter agreed, or the block fails saying they did not.
- **`any`** — the first verdict in `verdicts:` that anybody chose. `verdicts:` is an *ordered* list, so listing them most-to-least severe gives you "one objection is enough".
- **an agent name** — that agent judges, receiving every member's vote and note. It is an ordinary agent step, so its reasoning is recorded and inspectable; a judge that is also a voting member is a load error, because it would be marking its own homework.

### Two things that are never silent

- **A tie is an error.** With an even membership, or three verdicts and no majority, picking the first vote would be an invisible bug. Name an agent in `decide:` to break ties deliberately.
- **A member that ERRORS is not a member that voted.** A model or tool failure is not a "reject". By default one failed member fails the block; `member_errors: exclude` decides among the rest, and you have to ask for it — otherwise a three-agent ensemble silently becomes a two-agent one with a different meaning.

### The rest

- Members run **concurrently**, and each is its own merkle node: editing one member's prompt re-runs only that member.
- `verdicts:` lives on the **block**, not on members: it carries both the vocabulary and the block's routing. Every member votes in one vocabulary, and the block routes on the *decision*. Members are handed the names only, minus the reserved `failure:` catch.
- Every member's vote and note is recorded with the step's result, so a run's record says what was decided *and what it was decided from*.

## What's not on this page

The mechanics underneath an agent step — malformed tool-call repair, loop detection, OpenRouter prompt caching, and conversation compaction — are in [agents-internals.md](agents-internals.md). Reach for it when behavior surprises you; you don't need it to write a pipeline.
