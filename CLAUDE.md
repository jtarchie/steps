# CLAUDE.md: steps Pipeline Runner

## Overview

**steps** is a Go CLI that executes Concourse-style YAML pipelines with LLM agent integration. It discovers resources via templated shell commands, fetches versions, and runs jobs containing resource `get`/`put` steps and `agent` steps that invoke LLM models with tool-calling support (read_file, list_dir, run_shell, custom tools). State is persisted in SQLite with WAL mode. `steps run` executes one job once; `steps watch` polls `trigger: true` resources and automatically runs whichever jobs a version change affects, including versions produced by another job's own `put` — see "Downstream Triggers" below.

**Type:** CLI tool | **Language:** Go 1.26.5+ | **Size:** ~15K LOC | **Module:** `github.com/jtarchie/steps`

## Build & Test

### Prerequisites
- Go 1.26.5 or later
- golangci-lint v2.12+ (for linting; `brew install golangci-lint`)
- SQLite (via modernc.org/sqlite, vendored)

### Build
```bash
go build -v     # Produces ./steps binary (~52MB)
```

### Test
```bash
go test ./... -v              # Standard test run
go test ./... -v -p 1         # Serialized tests (deterministic package ordering; not required for correctness)
go test ./... -run TestName   # Single test
```
**Expected:** All tests pass in a few seconds; cache hits make subsequent runs instant. Verified (2026-07-15) with `-count=50` and a `-race -count=10` soak, both without `-p 1` and under artificial CPU saturation: no flakes. `-p 1` is a reasonable default for CI determinism, not a workaround for a known issue.

### Lint
```bash
golangci-lint run            # ~40 linters enabled; must pass with 0 issues before commit
```

### Typical workflow after code changes
```bash
go fmt ./...                 # Auto-format (most editors do this)
go mod tidy                  # Sync deps
golangci-lint run            # Catch issues early
go test ./...                # Validate logic (-p 1 optional; see Test Parallelism below)
go build -v                  # Final binary
```

## Known Constraints

### SQLite WAL Mode
The state database (`.steps/state.db`) is opened exactly once per process, at startup in `main.go`, and that single `*store.Store` handle is threaded through everything the process does — there is no re-opening and no concurrent `OpenStore` calls within one `steps` invocation. `OpenStore` (`internal/store/store.go`) sets both `busy_timeout` and `journal_mode=WAL` via DSN `_pragma` params on open, with no Go-side retry loop.

This is safe specifically *because* of the one-process-at-a-time model: converting a brand-new database file to WAL under **concurrent first access** can return `SQLITE_BUSY` that `busy_timeout` does not cover (confirmed empirically — ~15% failure rate over repeated concurrent-open trials against real SQLite, not a modernc.org/sqlite bug), but that only happens when multiple processes race to create the same file simultaneously, which never occurs here — `OpenStore` runs single-threaded before any step or agent work starts. If `steps` ever needs to support concurrent processes racing to create the same `.steps/state.db` (as opposed to concurrent steps *within* one already-running process, which is fine), the WAL conversion will need an explicit retry loop again — don't assume the DSN pragma alone is sufficient in that scenario.

`steps watch --max-concurrent N` (N > 1; see "Downstream Triggers" below) is the first case where this process actually does run concurrent goroutines against that single `*store.Store` handle — each triggered job's own `RecordNode`/`RecordJobRun`/queue writes, interleaved across workers. This is still safe with **no store change**: `OpenStore` calls `db.SetMaxOpenConns(1)`, so `database/sql` hands the one connection to a single goroutine at a time — a mutex, not a second `SQLITE_BUSY`-prone connection racing the WAL conversion above (that conversion already happened, once, at this same startup, before any worker exists). Don't add a connection pool or a second `OpenStore` path to "improve" concurrency here — that would reintroduce the exact concurrent-writer hazard the single-open model exists to avoid.

### Test Parallelism
No reproducible flakes were found under `go test ./... -race -count=10` and `-count=50`, both without `-p 1` and under artificial CPU saturation (verified 2026-07-15). `-p 1` remains a fine choice for deterministic CI output, but is not required to avoid failures.

## Project Layout

The module is a thin `main.go` entrypoint over a set of single-responsibility `internal/` packages, forming an acyclic dependency graph (each line below depends only on what's listed after `->`):

```
internal/shell, internal/template, internal/retry, internal/store, internal/config, internal/outcome   (leaves)
internal/resource, internal/workspace, internal/merkle                                -> config (+ shell/template for resource; merkle -> resource too)
internal/agent                                                                        -> config, store, merkle, workspace, outcome, retry, shell, template
internal/pipeline                                                                     -> config, merkle, outcome, resource, store, workspace, agent
internal/trigger                                                                      -> config, resource, store, workspace, pipeline
main.go                                                                               -> config, store, workspace, pipeline, trigger
```

### `internal/config` — the shared data model
YAML parsing (`LoadConfig`) and every config type: `Config`, `ResourceType`, `Resource`, `Agent`, `AgentSource`, `ToolSpec`, `Task`, `FixSpec`, `Job`, `Step`, `WorkspaceConfig`. Also owns the config-merge logic that both plan-time hashing and run-time execution call, so both stay in lockstep: `Config.ResolveTask` (a task step's `run:`/`fix:`, resolved against a top-level `tasks:` entry) and `Config.ResolveAgentInvocation` (an agent step's connection/dials/tool-grant, resolved against the named `agents:` entry). Depends on nothing but the standard library and `yaml.v3`.

### `internal/pipeline` — the orchestrator
`RunJob()` walks a job's plan in order: resolves/fetches `get` steps, runs `task`/`put`/`agent` steps, and records each step's outcome. It composes every other internal package; `internal/trigger` is the only package that depends on it, and only to call `RunJob` itself once per triggered job — the single-job semantics `RunJob` implements are otherwise untouched by that feature. `hooks.go` holds the step/job hook dispatch (`runHooks`/`runOneHook`/`runHookStep`) — see "Hooks" below.

### `internal/trigger` — cross-job downstream triggers
The cross-job counterpart to `internal/pipeline`'s single-job orchestration — see "Downstream Triggers" below for the full model. `Resources`/`AffectedJobs` read a `Config`'s `trigger: true` get steps; `Watch` runs two independent loops (a poller that diffs a resource's latest `check` version against `internal/store`'s `resource_checks` table and enqueues affected jobs into its `trigger_queue` table, and a worker pool that drains that queue via `pipeline.RunJob`) so a crash or a `--max-concurrent` > 1 pool doesn't lose track of pending work. `pollOnce`/`drainOne` are the unit-testable seams the loops are built from.

### `internal/agent` — agent step execution
Split by responsibility: `provider.go` (LLM client construction, persona/system-message building), `tools.go` (built-in + custom tool declarations and execution), `conversation.go` (the tool-calling request/execute/append loop), `step.go` (`RunStep` — the exported entrypoint an agent step in the plan runs through), `fix.go` (`RunFix` — a task's `fix:` agent, built on the same conversation machinery). Only `RunStep`/`RunFix` are exported; everything else is package-private.

### `internal/merkle` — content-addressed planning
`Node`/`Chain`/`PlanChains` plus the content-map builders (`GetNodeContent`/`TaskNodeContent`/`PutNodeContent`/`AgentContentMap`) shared between planning and real execution, so both compute identical hashes for identical steps. Depends on `config` and `resource` (to resolve a `get` step's version the same way at plan time and run time) — nothing execution-specific.

### `internal/resource` — resource type commands
Runs a resource type's `check`/`in`/`out` shell commands and selects among the versions a check returns (`ResolveVersions`, `SelectVersion`, `VersionMode`).

### `internal/workspace` — per-step/per-build filesystem views
`Provider`/`BuildWorkspace`/`StepSpace` interfaces; the default shared (single-directory) implementation; the `strategy: copy` backend (`workspace_copy*.go`, portable, copy-on-write via platform-specific `cp` flags) and `strategy: btrfs` backend (`workspace_btrfs*.go`, Linux only; subvolume create/snapshot/delete, with a non-Linux stub); static `inputs:`/`outputs:` plan validation (`ValidateArtifactFlow`).

### Leaves
- **`internal/store`** — SQLite state persistence (`Store`), WAL setup, `NodeRecord` (a persistence-shape copy of `merkle.Node`, so this package doesn't need to depend on `merkle`); also owns the downstream-trigger tables (`resource_checks`, `trigger_queue`) `internal/trigger` reads/writes through `Store` methods — see "Downstream Triggers" below
- **`internal/shell`** — shell command execution with context, logging, output truncation; `Runner` interface (`HostRunner` via `sh -c`, `DockerRunner` via a fresh `docker run --rm` per command) selected by `NewRunner(image string)` — see "Container Execution" below
- **`internal/template`** — YAML template rendering (e.g., `{{ .source.repo }}`)
- **`internal/retry`** — linear-backoff retry loop (`retry.Do`)
- **`internal/outcome`** — classifies a step/job error into `failed`/`errored`/`aborted` for hook dispatch (`Fail` marks a task-level failure; `Classify` buckets against the job ctx) — see "Hooks" below

### Configuration & Examples
- **.golangci.yml** — Linter config; 40+ rules including security (gosec), correctness, concurrency, complexity checks, and a `depguard` rule per package enforcing the dependency graph above
- **examples/review.yml** — Example pipeline: PR review job using an agent with `read_file`, `list_dir`, `run_shell`, and a custom `post_review` tool
- **examples/isolated.yml** — Example pipeline demonstrating opt-in `workspace:` isolation: a reusable task with `inputs:`/`outputs:`, an isolated agent step, and a `put` step scoped to a declared input
- **examples/container.yml** — Example pipeline demonstrating opt-in `image:` containerized execution: a resource_type, a top-level task (plus a step-level override), and an agent
- **examples/trigger.yml** — Example pipeline demonstrating downstream (cross-job) triggers: a self-contained (no network/credentials) `counter` resource type, a `publish` job that `put`s it, and a `notify` job whose `get ..., trigger: true` fires automatically under `steps watch` whenever `publish` lands a new version
- **examples/hooks.yml** — Example pipeline demonstrating step and job hooks (`on_success`/`on_failure`/`ensure`, incl. a nested hook) that doubles as a self-verifying fixture via `assert.execution`: a `passing` job (green, exits 0 under `steps run`) and a `failing` job whose `on_failure`/`ensure` hooks run and whose matching job `assert.execution` clears the failure. Run via `steps run examples/hooks.yml --job <name>` or `steps test examples/hooks.yml` (runs both jobs, checks every assert). Self-contained, no network/credentials
- **examples/subagent.yml** — Example pipeline demonstrating opt-in agent sub-delegation (`agent:` tools): an expensive `lead` agent grants a cheap `summarizer` agent as a callable tool and delegates bulk file-reading to it. Both point at local models so the shape is runnable offline. See "Agent Sub-Delegation" below

### Root Files
- **main.go** — CLI entry point; parses args into `run`/`watch`/`test` subcommands (`RunCmd`/`WatchCmd`/`TestCmd`) — `run` calls `config.LoadConfig()` → `pipeline.RunJob()`; `watch` → `trigger.Watch()` (see "Downstream Triggers"); `test` runs every job (force) and verifies `assert:` directives (see "Assert")
- **go.mod / go.sum** — Dependencies: yaml.v3, kong (CLI), tint (structured logging), modernc.org/sqlite, google.golang.org/genai, openai-go, golang.org/x/sys (btrfs backend)
- **.steps/** — State cache directory (created at runtime for each pipeline's working dir)

## CI & Validation

No GitHub Actions workflows are configured. Validate before pushing via:
```bash
golangci-lint run && go test ./... && go build -v
```

## Key Implementation Details

### Agent Execution Flow
1. Parse agent config (model endpoint, system prompt, tools, max_turns)
2. Build system message combining persona + working directory context
3. Enter tool-calling loop (up to `max_turns`; default 8):
   - Send message + tool definitions to model
   - If model requests tools, execute them (read_file, list_dir, run_shell, custom)
   - Truncate output to 100KB max to avoid context overflow
   - Return tool results to model
4. Exit loop when model stops requesting tools or max_turns exceeded
5. Record output and return

### Custom Tool `required:` Semantics
A custom tool (a `tools:` entry with `name`/`description`/`run`) may set `required: true` (see `examples/review.yml`'s `post_review`). This marks it as an action that must *succeed* before the step can complete — but **no tool failure, required or not, ever aborts or restarts the conversation.** A failed call (nonzero exit, or the command failing to run at all) always comes back to the model as ordinary data (`{exit_code, stdout, stderr}` or `{"error": ...}`), exactly like `run_shell`, so the model sees what went wrong and can recover in the same session — no cold restart, no wasted turns re-reading files or re-running exploration.

### Top-level `tasks:` Reuse
A top-level `tasks:` list (mirroring `resources:`/`agents:`) lets a `run:`/`fix:` pair be defined once and reused across jobs (see `examples/self-heal.yml`). A job's `task:` step is disambiguated by whether it carries its own `run:`:
- **`run:` present** → the step is inline, exactly as before; `tasks:` is never consulted, even if a same-named entry exists there.
- **`run:` absent** → `task:` instead names a `tasks:` entry, and its `run`/`fix` are used. The step's own `fix:`, if set, overrides the referenced task's `fix:` for that step only — everything else comes from the top-level definition.

This resolution (`Config.ResolveTask` in `internal/config`) runs identically at plan time (`internal/merkle`'s `planNonGetNode`/`taskNode`) and run time (`internal/pipeline`'s `runTaskStep`/`runTaskCommand`), so a task's merkle hash is always computed from its *resolved* `run:` string — an inline task's hash is unaffected by this feature. An undefined reference surfaces as an ordinary `FindTask` error at plan time, the same as an unknown `get`/`put`/`agent` name. An agent step's connection/dials/tool-grant are resolved the same way, by `Config.ResolveAgentInvocation`, called from `internal/merkle`'s `agentNode` and `internal/agent`'s `prepareAgentStep`/`RunFix`.

`required: true` is enforced entirely in `internal/agent`'s `runAgentConversation` (conversation.go) by tracking **success** (`exit_code == 0`), not mere invocation:
- **The model tries to stop while a required tool hasn't yet succeeded** → its next turn is constrained via the provider's `tool_choice` (`forceRequiredTool`, mapped by `genaiopenai` to OpenAI's named `tool_choice`) to a function call for that specific tool — a hard API-level constraint, not a text reminder the model could ignore. Some OpenAI-compat local servers (LM Studio confirmed; Ollama assumed similarly limited) 400 on that named-object `tool_choice` form, only accepting the string values `none`/`auto`/`required`. `AgentSource.StringToolChoice` (YAML `string_tool_choice:`, `*bool`) controls which form is sent; unset, it defaults to `!requiresKey` for a resolved provider (so `lmstudio`/`ollama` get the generic string `"required"` fallback, cloud providers keep the precise named force) or `false` for an explicit `endpoint:`. The fallback only guarantees *some* tool call, not the missing one specifically — the `max_turns` safety bound below still applies if the model calls the wrong tool repeatedly.
- **A forced (or voluntary) call still fails** → that failure is appended to the conversation like any other tool result. The model gets another turn to fix it and try again — no attempt is aborted for this.
- **The safety bound**: if a provider doesn't honor `tool_choice`, or the model just can't get the required tool to succeed, `max_turns` still caps the loop and the step fails, naming the tool(s) that never succeeded.
- `retry.Do`/`attempts:` (a full conversation restart from the original prompt) still exists, but only fires for *non-tool* failures — `generateOnce` erroring (LLM/transport issue) or `max_turns` exhaustion — never for a tool's own failure.
- Only custom tools can be marked `required:`; built-ins (`read_file`, `list_dir`, `run_shell`) and the fix-agent's injected task-rerun tool are never required — they're intentionally exploratory/iterative regardless.

### Agent Sub-Delegation (opt-in `agent:` tools)

A `tools:` entry may be a **sub-agent tool** — `{ agent: <name>, description: <text> }` — instead of a builtin name or a custom `{name, run}` tool. It exposes another `agents:` entry to the parent model as a callable tool named for that agent, taking a single `request` string. Each call runs the child's *own* fresh tool-calling conversation (its own model, persona, dials, `max_turns`, tool grant) and returns its final text as the tool result. This is "delegate and get an answer back" — categorically distinct from a job/resource handoff — and it touches only `internal/config` + `internal/agent` + `internal/merkle`; the plan, `trigger_queue`, and `RunJob` are untouched. Absent, behavior (and merkle hashes) are byte-identical to before this feature existed. See `examples/subagent.yml`.

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

- **Execution** (`internal/agent/subagent.go`, `buildSubAgentTool`/`preparedSubAgent.run`): the child runs in the **caller's** working directory (`env.dir`) but under the **child's own** resolved image (its `Agent.image:`), not the parent's — a sub-agent is a different worker, unlike a fix agent (which must reproduce the *failing task's* image, see Container Execution). `read_file`/`list_dir` stay host-side as always. The child's LLM client and (recursively) its own tool tree are built **eagerly** during the parent step's `prepareAgentStep`, so a missing credential or bad grant for a granted sub-agent fails preparation, not first-call.
- **No recording.** A child conversation gets no merkle node, no `job_run`, no execution-log entry — the same no-record contract as `fix:` agents (`RunFix`) and hook steps. The enclosing agent step records the aggregate outcome; the parent's own *call* of the sub-agent tool is what appears in its trajectory.
- **Failure is data.** A child failure (transport error, `max_turns` exhausted, a child required tool never succeeding) comes back to the parent as `{"error": ...}` — exactly like any other tool failure (see Custom Tool `required:` Semantics), never a Go error that aborts the parent conversation.
- **Selection & grant.** A sub-agent is a capability grant, like `run_shell`: a step selects a *granted* one by bare name (`tools: [summarizer]`, resolved through `resolveEffectiveTools` — the bare name substitutes the granted spec) and **cannot introduce one inline** on a step (rejected at `LoadConfig`). `ToolSpecName` returns the agent name for a sub-agent spec, so `grantedToolIndex` keys it correctly.
- **Load-time graph checks** (`Config.validateAgentGraph`): a sub-agent tool must set no `builtin`/`name`/`run`, can never be `required:`, and must reference an existing agent (unlike step-level agent refs, this one *is* cross-checked at load — the DFS needs it). The agent graph is walked depth-first for **cycles** (`reviewer → summarizer → reviewer` fails naming the path) and a **nesting-depth cap** (`maxSubAgentDepth = 8`), mirroring secret-agent's model. A **fix agent may not grant sub-agents** (`validateFixAgentSubAgents`) — nested delegation inside the fix loop is out of scope for v1.
- **Merkle** (`toolSpecsContent`/`subAgentInvocationContent` in `internal/merkle/merkle.go`): a sub-agent tool folds in the child's **resolved invocation content** (model/endpoint/persona/dials/max_turns/image + its own tools, recursively) so editing a child — or a grandchild — busts the parent step's hash. Value-gated exactly like the tool content it replaces: a non-sub-agent tool hashes as `{builtin, name, description, run}` byte-for-byte as before, so a pipeline with no sub-agent tools is unaffected. The child's prompt/dir/inputs/outputs/assert/hooks are *not* part of its identity (a sub-agent has no step), and the API key/env var are excluded (nothing secret-adjacent in hashed content), matching `AgentContentMap`.

### Workspace Isolation (opt-in)

By default every step in a triggered build shares one mutable directory: a `get` fetches into `<workspace>/<resource-name>/`, and every `task`/`agent`/`put` step after it runs with that same directory as its cwd, so one task can silently corrupt state for a later step. A top-level `workspace:` block opts a pipeline into Concourse-style per-step isolation instead; absent, behavior (and merkle hashes) are byte-identical to before this feature existed.

```yaml
workspace:
  strategy: copy   # or: btrfs (Linux only)
  root: /path       # optional for copy; required for btrfs
  options:
    compression: zstd   # btrfs only: zstd | lzo | zlib | none
```

- **`inputs:`/`outputs:`** on a `task`/`agent`/`put` step (and on a top-level `tasks:` entry, overridable per step the same way `fix:` is) name artifacts — a resource an earlier `get` fetched, or an output an earlier `task`/`agent` produced. A step sees *only* what it declares: an isolated task/agent's working directory contains an `<input>/` copy (or, on btrfs, an instant CoW snapshot) of each named input plus an empty `<output>/` dir per declared output, captured back into the build's artifact store after the step succeeds. `put` steps compose a read view the same way, from their own `inputs:`; there is no implicit "all artifacts so far" view.
- This is corruption hygiene, not a sandbox: shell commands can still reach outside the materialized directory via absolute paths, same as today.
- `Provider`/`BuildWorkspace`/`StepSpace` (`internal/workspace/workspace.go`) are the abstraction: a `Provider` is built once per CLI invocation (`workspace.NewProvider`) and validated at startup (`Validate()` — wrong platform, wrong filesystem, missing binaries all fail fast, before any step runs); `NewBuild` creates one triggered build's artifact store; `TaskSpace`/`PutSpace` materialize a step's directory; `Capture` persists declared outputs back into the store. The shared (no-`workspace:`) implementation makes every method a no-op/passthrough to the single directory.
- Fix agents (`fix:`) run inside the failing task's own already-materialized `StepSpace`, not a fresh one — they need to see the exact state the task failed in, and the enclosing task's `Capture` (after the fix loop's final green verdict) is what actually persists outputs downstream.
- Declaring `inputs:`/`outputs:` without a `workspace:` block is a `LoadConfig`-time error; an `inputs:` naming an artifact nothing earlier in the plan fetched/produced is a `RunJob`-time error (`workspace.ValidateArtifactFlow`) that runs unconditionally, even under `--force` (which otherwise skips merkle planning).
- Merkle hashes (`TaskNodeContent`/`PutNodeContent`/`AgentContentMap`, all in `internal/merkle/merkle.go`) fold in `inputs:`/`outputs:` **only when `cfg.Workspace != nil`** — so switching a pipeline into isolated mode invalidates its cache (correctly: the executed step's inputs changed), but a pipeline that never opts in hashes exactly as it always has. The workspace `strategy`/`root`/`options` themselves are never hashed — copy and btrfs produce the same logical view, so switching backends must not invalidate anyone's cache.

### Container Execution (opt-in `image:`)

By default every pipeline-defined command (`resource_type` `check`/`in`/`out`, a task's `run:`, an agent's `run_shell`/custom tools) runs on the host via `sh -c` (`internal/shell.HostRunner`). Setting `image:` on a `resource_types:` entry, a top-level `tasks:` entry, or an `agents:` entry runs that entity's commands in a fresh `docker run --rm --init` container from that image instead — one container per command, not a long-lived one. Absent, behavior (and merkle hashes) are byte-identical to before this feature existed.

```yaml
resource_types:
- name: git-alpine
  image: alpine/git       # check/in/out run in this image
  config: { check: ..., in: ..., out: ... }

tasks:
- name: build
  run: go build ./...
  image: golang:1.26      # this task's run: (and fix-loop re-runs) run in this image

agents:
- name: reviewer
  image: python:3.12      # this agent's run_shell/custom tools run in this image
```

- **Step-level override**: a `task`/`agent` step's own `image:` overrides the referenced `tasks:`/`agents:` entry's image for that step only — the same override idiom `fix:` uses. It's inherit-only: a non-empty step `image:` always wins; there's no way to force host execution from a step when the task/agent sets one. `image:` is invalid on `get`/`put` steps (a put's image comes from its resource type) — rejected at `LoadConfig` time.
- **`internal/shell.Runner`** is the abstraction: `HostRunner` (today's `sh -c` behavior) or `DockerRunner`, selected by `shell.NewRunner(image, cwd string) (Runner, error)` — every shell-out call site (`internal/resource`'s check/in/out, `internal/pipeline`'s task `run:`, `internal/agent`'s `shellToolResult` behind `run_shell`/custom tools) funnels through this single decision point. `cwd` is bound at construction, not passed again on `Run`/`RunCapture`/`RunCaptureFull` — so a runner reused across many calls against the same directory (an agent conversation's repeated `run_shell` calls, a task's fix-loop re-runs) resolves/validates its working directory once, not once per call. `NewRunner` can fail (e.g. `resolveMountPath` rejecting a `cwd` containing `:`, which would break docker's `-v host:container` volume-spec parsing) — every caller propagates that error.
- **Container shape**: `docker run --rm --init [-i] -v <cwd>:<cwd> -w <cwd> <image> sh -c <command>` — the working directory is bind-mounted at its own resolved (absolute, symlink-free) host path and set as the container's workdir, so host-side readers of the same directory (an agent's `read_file`/`list_dir`, which stay host-side `os.ReadFile`/`os.ReadDir`; `workspace.StepSpace.Capture`) see exactly what a containerized command wrote. `-i` is passed only where the host path wires stdin through (`Run`/`RunCapture`); `RunCaptureFull` (backing agent tool calls) never attaches stdin or a tty, matching its host counterpart's `/dev/null` semantics. No host environment variables are passed into the container — it starts from the image's own env only.
- **Exit codes pass through unchanged**: `docker run`'s own exit code (including docker-level failures — commonly 125 for a daemon-side error, 126/127 for a command the container couldn't run/find) is treated exactly like a host command's exit code by every caller — a hard error for `Run`/`RunCapture`, ordinary `{exit_code, stdout, stderr}` data for `RunCaptureFull` (so a bad image surfaces to an agent as data it can react to, not a crash).
- **Fix agents run under the failing task's image**, not the fix agent's own `Agent.image:` — `agent.RunFix`'s signature takes the task's `config.ResolvedTask` (carrying `Image`) precisely so its `run_shell`/custom tools and the injected task-rerun tool reproduce the exact environment that produced the failure; running the fix loop under a different image than the verdict re-run would make "fixed" meaningless. Since a fix agent's own `image:` can therefore never take effect, `Config.validateFixAgentImages` rejects it at `LoadConfig` time instead of silently ignoring it.
- **Fail-fast validation**: if any `image:` is set anywhere in the config (`Config.UsesImages()`), `pipeline.RunJob` calls `shell.ValidateDocker` (docker on `PATH` + `docker info` succeeds) before planning/executing anything — mirroring `workspace.Provider.Validate()`'s fail-fast precedent.
- **Merkle hashing**: `image` is folded into `TaskNodeContent`/`AgentContentMap`/`GetNodeContent`/`PutNodeContent` **whenever it's non-empty** — unlike `inputs:`/`outputs:` (gated on `cfg.Workspace != nil`, since their *relevance* depends on the workspace feature existing), an image change alters what a command actually executes against regardless of workspace mode, so the gate is on the value itself. A pipeline that never sets `image:` still hashes byte-identically to before this field existed.
- **Known caveats** (documented, not solved in v1): on Linux, a container's default (often root) user can leave root-owned files in the bind-mounted step directory, which can complicate `workspace` cleanup; a hard-killed docker CLI client (past its 10s SIGTERM grace period) can leave an orphaned container running until the daemon reaps it; there's no way to override a step back to host execution once its task/agent sets an image.

### Downstream Triggers (opt-in `trigger: true` + `steps watch`)

By default `steps` is a one-shot, single-job CLI (`steps run pipeline.yml --job x`): a `get` step's `trigger: true` is parsed but had no runtime effect before this feature, and there was no way for one job's `put` to cause a different job to run. `steps watch pipeline.yml` adds a long-running mode that polls every resource named by any `get ..., trigger: true` step, across every job in the pipeline, and automatically runs whichever jobs are affected when that resource's latest version changes — including a version produced by another job's own `put`, not just an externally-discovered one. Absent (i.e. under `steps run`), behavior is byte-identical to before this feature existed.

```yaml
jobs:
- name: publish
  plan:
  - put: some-resource
- name: notify
  plan:
  - get: some-resource
    trigger: true   # steps watch runs this job automatically when publish's put lands a new version
  - task: announce
    run: ...
```
```bash
steps watch pipeline.yml --interval 30s --max-concurrent 1
```

See `examples/trigger.yml` for a runnable, self-contained (no network/credentials) demonstration.

- **Two independent loops, connected only through `internal/store`'s durable queue** (`internal/trigger.Watch`): a **poller** calls `resource.CheckVersions` (the same check every `get` step already uses) for every trigger resource on `--interval`, diffs the latest version's JSON against `resource_checks` (a new table, one row per resource), and — on a change — enqueues every affected job (`AffectedJobs`) into `trigger_queue`. A **worker pool** (`--max-concurrent`, default 1) drains that queue by calling `pipeline.RunJob` exactly as `steps run` would. Splitting these into a durable, SQL-backed queue (rather than an in-memory dedup set) is what makes a crash mid-run not lose track of pending work, and gives `--max-concurrent` a real meaning — see `pollOnce`/`drainOne` in `internal/trigger/trigger.go`, the unit-testable seams the two loops are built from.
- **At-least-once, never at-most-once**: `pollOnce` advances a resource's recorded version (`RecordCheckedVersion`) **only after** every job that version's change affects has been durably enqueued. So if a later resource's check errors, or an `EnqueueJob` fails, or the process crashes mid-poll, the resource stays "dirty" and the trigger is retried on the next poll rather than silently consumed. `checkResource` is deliberately side-effect-free (it does not record) for exactly this ordering.
- **Cold start seeds a baseline, never triggers.** A resource checked for the first time ever (no `resource_checks` row) just records its current version — it is never itself considered "dirty" on that first check. This keeps a fresh (or freshly lost) `.steps/state.db` from mass-re-running every job the moment `watch` starts; only a *subsequent* change triggers anything.
- **`trigger_queue` dedup, ordering, and per-job serialization**: a partial unique index (`WHERE status = 'pending'`) means a resource going dirty twice before a worker claims the row enqueues its affected job once, not twice — but a job already `running` can still get a fresh `pending` row queued behind it, so a version change mid-run isn't dropped. `ClaimNextJob` (a single `UPDATE ... RETURNING`, oldest `pending` first) additionally **won't claim a pending row whose job already has a `running` row**, so builds of the same job never overlap even with `--max-concurrent > 1` — a mid-run change runs *after* the in-flight build finishes, not concurrently. Two workers can never claim the same row, and no additional locking is needed, riding `Store`'s existing `SetMaxOpenConns(1)` (see "SQLite WAL Mode" above). `ResetStaleRunning` accounts for a `running` + `pending` pair for one job at crash time (it drops the superseded `running` row rather than creating a second `pending` one, which would violate that unique index).
- **Graceful-shutdown carve-out**: a job *interrupted* by `ctx` cancellation (SIGINT/SIGTERM mid-run — a nonzero `RunJob` return with `ctx` already canceled) is *not* marked `failed` — that would silently drop it forever, since only a new version change would otherwise ever re-trigger it. Its row is left `running`; `ResetStaleRunning` (called once at every `watch` startup) flips any such stranded row back to `pending`, recovering both a hard crash and an interrupted graceful shutdown the same way. A job that instead *reached a terminal state* — `done`, or a genuine failure with `ctx` still live — is finalized with `context.WithoutCancel(ctx)`, so a SIGINT racing the completion still records the true outcome instead of stranding the row `running` and causing a spurious re-run. A genuine failure is not retried until the next real version change re-enqueues it — the same non-retry semantics `job_runs`/merkle skip-caching already gives `steps run`.
- **No `passed:`-style version-set gating across jobs** — that Concourse concept doesn't exist anywhere in this codebase's model, so there's no "only trigger B with the exact version set that passed through A" to preserve; any dirty resource simply enqueues every job with a matching `trigger: true` get step.
- **CLI**: `steps run <pipeline.yml> [--job x] [--force]` (today's behavior, unchanged) and `steps watch <pipeline.yml> [--interval 30s] [--max-concurrent 1] [--force]` are Kong subcommands (`RunCmd`/`WatchCmd` in `main.go`); `run` is `default:"withargs"`, so the pre-existing flat invocation (`steps pipeline.yml --job x`, flags before or after the positional path) keeps parsing identically with no call-site changes required.

### Hooks (opt-in `on_success`/`on_failure`/`on_error`/`on_abort`/`ensure`)

Any plan step or whole job can carry Concourse-style hooks that react to its outcome. A hook is itself a full step (task/put/agent — never `get`, rejected at `LoadConfig`), so it can `run:` a command, `put:` a resource, or invoke an `agent:`, and may recursively carry its own hooks. Absent, behavior (and merkle hashes) are byte-identical to before this feature existed. See `examples/hooks.yml`.

```yaml
jobs:
- name: build
  plan:
  - task: work
    run: ./build.sh
    on_failure:                # step-level hook
      task: notify
      run: ./notify.sh failed
    ensure:
      task: cleanup
      run: ./cleanup.sh
  on_success:                  # job-level hook (inline alongside plan:)
    put: status
```

- **Observer semantics, never consumers.** A failing step's `on_failure` runs, then the failure **still propagates** — the job fails, `steps run` exits nonzero, `steps watch` records the run `failed`. This is the opposite of the pocketci prior art (where a hook consumed the error): a hook never clears a failure (a job-level `assert.execution` does that instead — see "Assert" below). The one way a hook changes an outcome is upward: a failing `on_success` or `ensure` hook fails an otherwise-green step/job (a broken notification/cleanup shouldn't read as success). A failing `on_failure`/`on_error`/`on_abort` hook is only logged — the outcome was already failing.
- **Classification** (`internal/outcome`): every step error is bucketed against the *job* context — **failed** (a task-level failure: a nonzero command exit, a fix verdict still red, or an agent's required tool never succeeding — marked via `outcome.Fail`/`shell.IsExitError` at the producing site), **errored** (everything unmarked: workspace setup, docker, LLM transport, template, store, a resource `check`/planning failure), or **aborted** (`ctx.Err() != nil` — a SIGINT/SIGTERM mid-run). Classification keys on `ctx.Err()` **first**, so an agent step's own internal `WithTimeout` never misreads as an abort while the job ctx is live. Caveat: a docker-level exit (125/126/127) classifies as *failed*, since `docker run`'s exit code is indistinguishable from the command's.
- **Ordering**: the single matching `on_*` hook runs, then `ensure` always runs last, regardless of outcome. `on_abort`/`ensure` reached after cancellation run under `context.WithTimeout(context.WithoutCancel(ctx), 60s)` (the `hookGracePeriod` in `internal/pipeline/hooks.go`) so they complete detached from the canceled context but not forever — the same `WithoutCancel` idiom `internal/trigger` uses to finalize an interrupted job.
- **No merkle/store identity of their own.** A hook step executes with **no** merkle node and **no** `job_run`/node recording — the same no-record contract as a task's `fix:` agent (`agent.RunFix`). The enclosing step/job records the aggregate outcome. `runHooks`/`runOneHook`/`runHookStep` (`internal/pipeline/hooks.go`) are the dispatch seam; task/put/agent hook bodies reuse `executeTask`/`executePut`/`agent.RunHook`, extracted from the recording step runners.
- **Step hooks fold into the step's content hash** (`merkle.withHooks`, value-gated exactly like `image:`): a step with no hooks hashes byte-identically to before, but editing a hook — or the top-level `tasks:`/`agents:` entry it references, since the hook's content is its *resolved* form — busts the parent step's cache. A **skipped (cached) step fires no hooks** (it didn't run). `Chain.Unskippable` is unchanged: hooks don't make a task unskippable, because a non-run step has no observers to fire.
- **Job hooks are never hashed** and fire on **every** `RunJob` invocation (even a fully-cached run), since they don't alter what any step executes. They run in the `RunJob`-level build workspace — which for a get-leading plan is empty (each triggered build has its own) — so a **job-level hook may not declare `inputs:`/`outputs:`** (rejected at `LoadConfig`). Step-level hooks keep full `inputs:`/`outputs:` support: an `on_success` hook validates its inputs against the step's post-outputs view, failure-path hooks against the pre-outputs view, and a hook's own outputs are captured but never satisfy a later plan step (`workspace.validateStepHooks`).
- **`steps watch` needs no changes**: job hooks live inside `RunJob`, so `trigger.drainOne` gets them for free; an aborted job still returns the original ctx-canceled error after its grace-period hooks, preserving the graceful-shutdown carve-out.

### Assert (opt-in self-verification) + `steps test`

`assert:` lets a pipeline verify its own behavior — the mechanism that makes a hooks fixture a runnable test (steps' analog of pocketci's `all.yml`). Two shapes of a single `Assert` type (`internal/config`), context-validated by `validateAsserts`, absent = byte-identical to before. See `examples/hooks.yml`, run via `steps test examples/hooks.yml`.

- **`assert.execution`** on a **job** (ordered task/agent/hook names that must have run) or the **pipeline top level** (ordered job names). A job's execution is recorded into an in-memory `execLog` (`internal/pipeline/execlog.go`) carried through the invocation via a package-local `context.WithValue` — no signature churn, and recording happens only at pipeline dispatch points (`dispatchNonGetStep`, `runHookStep`, `runTriggeredBuild`), never inside `internal/agent`. **A matching job `assert.execution` clears the plan's failure** (evaluated in `RunJob` *after* hooks, so the log includes them): a fixture of deliberately-failing tasks stays green as long as the recorded order matches. A **mismatch fails the job** with a `want`/`got` diff and is never itself cleared. Execution asserts are **never hashed** (meta-checks, like job hooks).
- **`assert: {stdout, code}`** on a **task/agent step** (`code` task-only — agents have no exit code). The task runs through the capture path (like `fix:`), and a matching `stdout` substring + exact `code` make a **non-zero-exit task a success** (`assertMismatch` in `pipeline.go`); a mismatch is a task-level failure so `on_failure` fires. Evaluated before step hooks. Assert takes over success determination, so a task's `fix:` is not consulted when an assert is present. A step assert **is** folded into the node's content hash (value-gated like `image:`, via `merkle.assertContent`) — it changes the success criteria, so it must bust the cache.
- **`steps test <pipeline.yml>`** (`TestCmd` in `main.go`): runs **every** job in declaration order with `skipCache=true` (force, so the execution log is deterministic), prints per-job `PASS`/`FAIL`, and checks the pipeline-level `assert.execution` (job names). Exits non-zero if any job errored or the pipeline assert mismatched. This is the self-verifying-fixture entry point.
- **Deferred**: an `on_error` fixture needs a docker bad-image task (host `sh -c missing` is exit 127 = *failed*, not errored); an `on_abort` fixture needs a per-task `timeout:` directive. The `assert`/hook machinery already classifies and dispatches both — only the deterministic *triggers* are missing, so the shipped fixture covers success/failure/ensure on the host.

### State Caching via Merkle Tree
- Each resource `get` and `agent` step is content-addressed (merkle tree) with its inputs (pinned versions, source config)
- After successful execution, the merkle root is stored in SQLite
- On subsequent runs, if inputs haven't changed (same versions, same configs), the step is skipped
- Use `--force` flag to bypass caching and re-run everything

### Template Rendering
Resource check/in/out commands and agent custom tools support `{{ .source.* }}` and `{{ .version.* }}` templating for dynamic command construction (e.g., `gh pr list --repo {{ .source.repo }}`). A custom tool's `run:` additionally sees the model's call arguments as `{{ .args.* }}` (see `inferToolParams` in `internal/agent/tools.go`, which derives the tool's parameter schema from these references).

Templates have the full [slim-sprig](https://github.com/go-task/slim-sprig) function library available (`sprig.TxtFuncMap()`, merged in `internal/template/template.go`'s `newFuncMap`) — string/list/default/date helpers, dependency-free — plus our own `shellquote`. Two non-stdlib imports are allowed into `internal/template` by `.golangci.yml`'s depguard: `slim-sprig/v3` and `leatherman/pkg/shellquote`.

Since a rendered template runs via `sh -c`, any value interpolated into it that could contain shell metacharacters — backticks, `$(...)`, quotes, `; | &` — must be piped through the `shellquote` function (`internal/template/template.go`, backed by `github.com/frioux/leatherman/pkg/shellquote`), which renders it as one safely-quoted POSIX word, quoting only when the value needs it and supplying its own quotes — so don't add surrounding `"..."`: `-b {{ .args.body | shellquote }}`. This matters most for LLM- or PR-authored values (e.g. a review body): without it, a body containing `` `replace` `` gets command-substituted by the shell and posted with those words missing. See `examples/review.yml`'s `post_review` tool.

## Guidance for Claude Agents

When making changes:
1. **Trust the instructions.** Verify a command only if the docs are incomplete or proven wrong in testing.
2. **Golangci-lint first.** If linting fails, the build is rejected; fix linter errors before logic changes.
3. **Run the full sequence** before committing: `go fmt ./... && go mod tidy && golangci-lint run && go test ./... && go build -v`.
4. **If touching `internal/store/store.go`**: `OpenStore` is called exactly once per process and that single handle is threaded everywhere — don't add a second `OpenStore` call path or a per-object pool, even to "help" `steps watch --max-concurrent`'s worker pool (it's already safe via `SetMaxOpenConns(1)`; see "SQLite WAL Mode" above). WAL is set via DSN pragma with no retry loop, which is only safe because of that single-open-at-startup model.
5. **Keep `internal/agent`'s conversation loop's behavior stable.** The tool-calling loop (`conversation.go`'s `runAgentConversation`) is tightly coupled to context propagation, logging, and `required:` enforcement via `tool_choice` forcing; file-organization changes within the package are fine, but refactors must preserve the loop's exact semantics — see "Custom Tool `required:` Semantics" above.
6. **Respect the package dependency graph** (see Project Layout above and `.golangci.yml`'s `depguard` rules): `internal/config` depends on nothing internal; `internal/pipeline` is the only package that depends on `internal/agent`; `internal/trigger` is the only package (besides `main`) that depends on `internal/pipeline`, and only to call `RunJob` per triggered job. A new import that isn't in a package's `depguard` allow-list is a signal the change belongs in a different package, not a rule to route around.
