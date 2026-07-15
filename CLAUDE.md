# CLAUDE.md: steps Pipeline Runner

## Overview

**steps** is a Go CLI that executes Concourse-style YAML pipelines with LLM agent integration. It discovers resources via templated shell commands, fetches versions, and runs jobs containing resource `get`/`put` steps and `agent` steps that invoke LLM models with tool-calling support (read_file, list_dir, run_shell, custom tools). State is persisted in SQLite with WAL mode.

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
go test ./... -v              # Standard test run (may have minor flakes under CPU load)
go test ./... -v -p 1         # Serialized tests (use this for CI reliability; known flakes under parallel load)
go test ./... -run TestName   # Single test
```
**Expected:** All tests pass in <5s with `-p 1`, cache hits make subsequent runs instant.

### Lint
```bash
golangci-lint run            # ~40 linters enabled; must pass with 0 issues before commit
```

### Typical workflow after code changes
```bash
go fmt ./...                 # Auto-format (most editors do this)
go mod tidy                  # Sync deps
golangci-lint run            # Catch issues early
go test ./... -p 1           # Validate logic
go build -v                  # Final binary
```

## Known Constraints

### SQLite WAL Mode
The state database (`.steps/state.db`) uses SQLite's WAL (Write-Ahead Logging) mode. **Do not** rely solely on `busy_timeout` or `MaxOpenConns(1)`—WAL requires explicit retry loops in concurrent scenarios. See `internal/store/store.go` for the pattern. Recent modernc.org/sqlite versions handle this correctly; if a test flakes with "database is locked," ensure retries are in place, not just timeout config.

### Test Parallelism
Under high CPU contention (many tests in parallel), some shell/storage tests flake intermittently. Always run `go test ./... -p 1` in CI or when validating changes. Standard `go test` may pass locally but fail under load. This is **not** a bug; it's a known interaction between test timing and SQLite's internal locking.

## Project Layout

The module is a thin `main.go` entrypoint over a set of single-responsibility `internal/` packages, forming an acyclic dependency graph (each line below depends only on what's listed after `->`):

```
internal/shell, internal/template, internal/retry, internal/store, internal/config   (leaves)
internal/resource, internal/workspace, internal/merkle                                -> config (+ shell/template for resource; merkle -> resource too)
internal/agent                                                                        -> config, store, merkle, workspace, retry, shell, template
internal/pipeline                                                                     -> config, merkle, resource, store, workspace, agent
main.go                                                                               -> config, store, workspace, pipeline
```

### `internal/config` — the shared data model
YAML parsing (`LoadConfig`) and every config type: `Config`, `ResourceType`, `Resource`, `Agent`, `AgentSource`, `ToolSpec`, `Task`, `FixSpec`, `Job`, `Step`, `WorkspaceConfig`. Also owns the config-merge logic that both plan-time hashing and run-time execution call, so both stay in lockstep: `Config.ResolveTask` (a task step's `run:`/`fix:`, resolved against a top-level `tasks:` entry) and `Config.ResolveAgentInvocation` (an agent step's connection/dials/tool-grant, resolved against the named `agents:` entry). Depends on nothing but the standard library and `yaml.v3`.

### `internal/pipeline` — the orchestrator
`RunJob()` walks a job's plan in order: resolves/fetches `get` steps, runs `task`/`put`/`agent` steps, and records each step's outcome. It composes every other internal package; nothing depends on it.

### `internal/agent` — agent step execution
Split by responsibility: `provider.go` (LLM client construction, persona/system-message building), `tools.go` (built-in + custom tool declarations and execution), `conversation.go` (the tool-calling request/execute/append loop), `step.go` (`RunStep` — the exported entrypoint an agent step in the plan runs through), `fix.go` (`RunFix` — a task's `fix:` agent, built on the same conversation machinery). Only `RunStep`/`RunFix` are exported; everything else is package-private.

### `internal/merkle` — content-addressed planning
`Node`/`Chain`/`PlanChains` plus the content-map builders (`GetNodeContent`/`TaskNodeContent`/`PutNodeContent`/`AgentContentMap`) shared between planning and real execution, so both compute identical hashes for identical steps. Depends on `config` and `resource` (to resolve a `get` step's version the same way at plan time and run time) — nothing execution-specific.

### `internal/resource` — resource type commands
Runs a resource type's `check`/`in`/`out` shell commands and selects among the versions a check returns (`ResolveVersions`, `SelectVersion`, `VersionMode`).

### `internal/workspace` — per-step/per-build filesystem views
`Provider`/`BuildWorkspace`/`StepSpace` interfaces; the default shared (single-directory) implementation; the `strategy: copy` backend (`workspace_copy*.go`, portable, copy-on-write via platform-specific `cp` flags) and `strategy: btrfs` backend (`workspace_btrfs*.go`, Linux only; subvolume create/snapshot/delete, with a non-Linux stub); static `inputs:`/`outputs:` plan validation (`ValidateArtifactFlow`).

### Leaves
- **`internal/store`** — SQLite state persistence (`Store`), WAL setup, `NodeRecord` (a persistence-shape copy of `merkle.Node`, so this package doesn't need to depend on `merkle`)
- **`internal/shell`** — shell command execution with context, logging, output truncation
- **`internal/template`** — YAML template rendering (e.g., `{{ .source.repo }}`)
- **`internal/retry`** — linear-backoff retry loop (`retry.Do`)

### Configuration & Examples
- **.golangci.yml** — Linter config; 40+ rules including security (gosec), correctness, concurrency, complexity checks, and a `depguard` rule per package enforcing the dependency graph above
- **examples/review.yml** — Example pipeline: PR review job using an agent with `read_file`, `list_dir`, `run_shell`, and a custom `post_review` tool
- **examples/isolated.yml** — Example pipeline demonstrating opt-in `workspace:` isolation: a reusable task with `inputs:`/`outputs:`, an isolated agent step, and a `put` step scoped to a declared input

### Root Files
- **main.go** — CLI entry point; parses args, calls `run()` → `config.LoadConfig()` → `pipeline.RunJob()`
- **go.mod / go.sum** — Dependencies: yaml.v3, kong (CLI), tint (structured logging), modernc.org/sqlite, google.golang.org/genai, openai-go, golang.org/x/sys (btrfs backend)
- **.steps/** — State cache directory (created at runtime for each pipeline's working dir)

## CI & Validation

No GitHub Actions workflows are configured. Validate before pushing via:
```bash
golangci-lint run && go test ./... -p 1 && go build -v
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
- **The model tries to stop while a required tool hasn't yet succeeded** → its next turn is constrained via the provider's `tool_choice` (`forceRequiredTool`, mapped by `genaiopenai` to OpenAI's named `tool_choice`) to a function call for that specific tool — a hard API-level constraint, not a text reminder the model could ignore.
- **A forced (or voluntary) call still fails** → that failure is appended to the conversation like any other tool result. The model gets another turn to fix it and try again — no attempt is aborted for this.
- **The safety bound**: if a provider doesn't honor `tool_choice`, or the model just can't get the required tool to succeed, `max_turns` still caps the loop and the step fails, naming the tool(s) that never succeeded.
- `retry.Do`/`attempts:` (a full conversation restart from the original prompt) still exists, but only fires for *non-tool* failures — `generateOnce` erroring (LLM/transport issue) or `max_turns` exhaustion — never for a tool's own failure.
- Only custom tools can be marked `required:`; built-ins (`read_file`, `list_dir`, `run_shell`) and the fix-agent's injected task-rerun tool are never required — they're intentionally exploratory/iterative regardless.

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

### State Caching via Merkle Tree
- Each resource `get` and `agent` step is content-addressed (merkle tree) with its inputs (pinned versions, source config)
- After successful execution, the merkle root is stored in SQLite
- On subsequent runs, if inputs haven't changed (same versions, same configs), the step is skipped
- Use `--force` flag to bypass caching and re-run everything

### Template Rendering
Resource check/in/out commands and agent custom tools support `{{ .source.* }}` and `{{ .version.* }}` templating for dynamic command construction (e.g., `gh pr list --repo {{ .source.repo }}`).

## Guidance for Claude Agents

When making changes:
1. **Trust the instructions.** Verify a command only if the docs are incomplete or proven wrong in testing.
2. **Test with `-p 1`.** All tests must pass serially; parallel flakes are expected and not a sign of correctness issues.
3. **Golangci-lint first.** If linting fails, the build is rejected; fix linter errors before logic changes.
4. **Run the full sequence** before committing: `go fmt ./... && go mod tidy && golangci-lint run && go test ./... -p 1 && go build -v`.
5. **Check WAL constraints** if touching `internal/store/store.go`: ensure retries are explicit, not just timeout config.
6. **Keep `internal/agent`'s conversation loop's behavior stable.** The tool-calling loop (`conversation.go`'s `runAgentConversation`) is tightly coupled to context propagation, logging, and `required:` enforcement via `tool_choice` forcing; file-organization changes within the package are fine, but refactors must preserve the loop's exact semantics — see "Custom Tool `required:` Semantics" above.
7. **Respect the package dependency graph** (see Project Layout above and `.golangci.yml`'s `depguard` rules): `internal/config` depends on nothing internal; `internal/pipeline` is the only package that depends on `internal/agent`; nothing depends on `internal/pipeline`. A new import that isn't in a package's `depguard` allow-list is a signal the change belongs in a different package, not a rule to route around.
