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
- **`internal/shell`** — shell command execution with context, logging, output truncation; `Runner` interface (`HostRunner` via `sh -c`, `DockerRunner` via a fresh `docker run --rm` per command) selected by `NewRunner(image string)` — see "Container Execution" below
- **`internal/template`** — YAML template rendering (e.g., `{{ .source.repo }}`)
- **`internal/retry`** — linear-backoff retry loop (`retry.Do`)

### Configuration & Examples
- **.golangci.yml** — Linter config; 40+ rules including security (gosec), correctness, concurrency, complexity checks, and a `depguard` rule per package enforcing the dependency graph above
- **examples/review.yml** — Example pipeline: PR review job using an agent with `read_file`, `list_dir`, `run_shell`, and a custom `post_review` tool
- **examples/isolated.yml** — Example pipeline demonstrating opt-in `workspace:` isolation: a reusable task with `inputs:`/`outputs:`, an isolated agent step, and a `put` step scoped to a declared input
- **examples/container.yml** — Example pipeline demonstrating opt-in `image:` containerized execution: a resource_type, a top-level task (plus a step-level override), and an agent

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
- **The model tries to stop while a required tool hasn't yet succeeded** → its next turn is constrained via the provider's `tool_choice` (`forceRequiredTool`, mapped by `genaiopenai` to OpenAI's named `tool_choice`) to a function call for that specific tool — a hard API-level constraint, not a text reminder the model could ignore. Some OpenAI-compat local servers (LM Studio confirmed; Ollama assumed similarly limited) 400 on that named-object `tool_choice` form, only accepting the string values `none`/`auto`/`required`. `AgentSource.StringToolChoice` (YAML `string_tool_choice:`, `*bool`) controls which form is sent; unset, it defaults to `!requiresKey` for a resolved provider (so `lmstudio`/`ollama` get the generic string `"required"` fallback, cloud providers keep the precise named force) or `false` for an explicit `endpoint:`. The fallback only guarantees *some* tool call, not the missing one specifically — the `max_turns` safety bound below still applies if the model calls the wrong tool repeatedly.
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
2. **Test with `-p 1`.** All tests must pass serially; parallel flakes are expected and not a sign of correctness issues.
3. **Golangci-lint first.** If linting fails, the build is rejected; fix linter errors before logic changes.
4. **Run the full sequence** before committing: `go fmt ./... && go mod tidy && golangci-lint run && go test ./... -p 1 && go build -v`.
5. **Check WAL constraints** if touching `internal/store/store.go`: ensure retries are explicit, not just timeout config.
6. **Keep `internal/agent`'s conversation loop's behavior stable.** The tool-calling loop (`conversation.go`'s `runAgentConversation`) is tightly coupled to context propagation, logging, and `required:` enforcement via `tool_choice` forcing; file-organization changes within the package are fine, but refactors must preserve the loop's exact semantics — see "Custom Tool `required:` Semantics" above.
7. **Respect the package dependency graph** (see Project Layout above and `.golangci.yml`'s `depguard` rules): `internal/config` depends on nothing internal; `internal/pipeline` is the only package that depends on `internal/agent`; nothing depends on `internal/pipeline`. A new import that isn't in a package's `depguard` allow-list is a signal the change belongs in a different package, not a rule to route around.
