# CLAUDE.md: steps Pipeline Runner

## Overview

**steps** is a Go CLI that executes Concourse-style YAML pipelines with LLM agent integration. It discovers resources via templated shell commands, fetches versions, and runs jobs containing resource `get`/`put` steps and `agent` steps that invoke LLM models with tool-calling support (read_file, list_dir, run_shell, custom tools). State is persisted in SQLite with WAL mode. `steps run` executes one job once; `steps watch` polls `trigger: true` resources and automatically runs whichever jobs a version change affects, including versions produced by another job's own `put` — see [docs/infra.md](docs/infra.md).

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

`steps watch --max-concurrent N` (N > 1; see [docs/infra.md](docs/infra.md)) is the first case where this process actually does run concurrent goroutines against that single `*store.Store` handle — each triggered job's own `RecordNode`/`RecordJobRun`/queue writes, interleaved across workers. This is still safe with **no store change**: `OpenStore` calls `db.SetMaxOpenConns(1)`, so `database/sql` hands the one connection to a single goroutine at a time — a mutex, not a second `SQLITE_BUSY`-prone connection racing the WAL conversion above (that conversion already happened, once, at this same startup, before any worker exists). Don't add a connection pool or a second `OpenStore` path to "improve" concurrency here — that would reintroduce the exact concurrent-writer hazard the single-open model exists to avoid.

### Test Parallelism
No reproducible flakes were found under `go test ./... -race -count=10` and `-count=50`, both without `-p 1` and under artificial CPU saturation (verified 2026-07-15). `-p 1` remains a fine choice for deterministic CI output, but is not required to avoid failures.

### Trust Boundaries Around Pipeline-Defined and Model-Directed Execution
Several areas of this codebase sit on the boundary between "the pipeline author wrote this" and "a model, or an external actor's data, can influence this" — a security review (2026-07-21) tightened a handful of these boundaries, and anyone touching the same areas again should know the constraints now in place rather than rediscovering them:

- **`internal/shell.HostRunner` does not inherit the full process environment.** `hostEnv()` (`internal/shell/shell.go`) filters `os.Environ()` down to a fixed allowlist (`hostEnvAllowlist`: `PATH`, `HOME`, locale/terminal vars, `TMPDIR`/`USER`/`SHELL`, `SSH_AUTH_SOCK`, proxy vars) before every host-executed command — resource `check`/`in`/`out`, a task's `run:`, and an agent's `run_shell`/custom tools. This is deliberate: an agent step's `run_shell`/custom tools are commands a model directs, and the full environment previously included every configured agent's `api_key_env` secret and anything else the operator exported. If a host-executed command in a new feature needs some other ambient variable, add it to `hostEnvAllowlist` explicitly and say why — don't reach for `os.Environ()`/`cmd.Env = nil` as a shortcut, and don't assume a pipeline can read an arbitrary exported credential just because the `steps` process can see it. There is currently no per-pipeline pass-through mechanism (e.g. an `env:` block) for a variable outside the allowlist; that would be a real feature (its own config surface, merkle-hash implications) if ever needed.
- **`image:` values are validated, and `dockerRunArgs` inserts a literal `--` before the image argument.** Both exist because an unvalidated, unterminated image string is an argv position docker's own flag parser can misread (`--privileged`, `-v /:/host`, etc.). If you add another code path that shells out to `docker run` with a pipeline-supplied image (rather than going through `shell.NewRunner`/`dockerRunArgs`), it needs the same `--` terminator; if you add another place `image:` can be set in the config schema, route it through `checkImageValue` (or the load-time chain that calls it) rather than skipping validation for the new field.
- **Workspace `materialize`/`Capture` reject a symlinked source directory** (`rejectSymlinkSrc` in `internal/workspace/workspace.go`, checked via `os.Lstat` before both an input's materialization and a step's output capture). A task or agent step under `workspace:` isolation fully controls its own materialized directory and could otherwise swap a declared output for a symlink to an arbitrary host path; the copy backend's `cp ... src/. dst` form dereferences a symlink at the top-level `src` argument regardless of `-P`. Any new `treeBackend` implementation (beyond `copy`/`btrfs`) inherits this guard for free as long as it goes through `isolatingSpace.Capture`/`isolatingBuild.materializeSpace` rather than calling a backend's `materialize` directly.
- **Agent `read_file`/`list_dir` path confinement is symlink-aware**, not just a lexical `filepath.Clean`/prefix check (`resolveAgentPath` → `rejectSymlinkEscape` in `internal/agent/tools.go`, mirroring `internal/shell/docker.go`'s `resolveMountPath`). `run_shell` itself still has no path confinement at all (by design — it's the escape hatch), so a model can plant a symlink inside the step's working directory via `run_shell` before calling `read_file`/`list_dir` on it; both now resolve symlinks and re-check containment before opening anything. A path that doesn't exist yet is intentionally *not* treated as an escape (there's nothing to leak, and the caller's own not-found error still surfaces).
- **The custom-tool arg-inference regex (`agentToolArgPattern` in `internal/agent/tools.go`) matches a piped reference**, e.g. `{{ .args.repo | shellquote }}`, not just the bare `{{ .args.NAME }}` form — the documented safe idiom for interpolating untrusted values (see [docs/templating.md](docs/templating.md)) would otherwise silently defeat the missing-argument validation it's meant to enable. If you change this regex again, verify it still matches the piped form; a regression here disables schema/required-argument enforcement specifically for tools written the recommended way, which is easy to miss in review since the tool still "works."
- **`AgentSource.Endpoint` may not embed userinfo** (`validateAgentEndpoints` in `internal/config/config.go`, checked at `LoadConfig` time). The resolved `BaseURL` — endpoint included — is folded into `AgentContentMap`/`subAgentInvocationContent` and persisted verbatim via `store.RecordNode`; a credential embedded directly in `endpoint:` (`https://user:token@proxy/`) would otherwise land in `.steps/state.db` in cleartext, contradicting the documented claim that hashed agent content excludes anything secret-adjacent (an exclusion that has only ever actually covered `api_key_env`'s name/value). Use `api_key_env` for credentials; `endpoint:` is a plain base URL.
- **Debug-level logging is opt-in, not the default.** `steps` accepts a global `--log-level`/`STEPS_LOG_LEVEL` flag (`debug`/`info`/`warn`/`error`, default `info` — see `main.go`'s `CLI.LogLevel`/`parseLogLevel`/`initLogging`). At debug level, `internal/shell`/`internal/shell/docker.go` log the full rendered command string and complete captured stdout/stderr — exactly the level at which a credential embedded in a templated `source:` or printed by a CLI tool would show up on stderr. Don't lower the default back to debug, and if you add new debug-level logging of command/output content, assume it's off by default and only shown when an operator explicitly opts in.
- **MCP OAuth tokens live outside `.steps/` and are never merkle-hashed**, the same treatment `api_key_env` values already get. `MCPServer.Endpoint`/`Auth.Type`/`Auth.APIKeyEnv` (the env var *name*, never its value) are folded into hashed content and `mcp_servers:`'s own `validateMCPServers` rejects userinfo in `endpoint:` exactly like `validateAgentEndpoints` does — but the actual OAuth access/refresh token an `auth: {type: oauth}` server needs is a second, distinct kind of secret with no `api_key_env`-style env-var escape hatch (it's obtained interactively, not supplied by the operator). It's persisted to `${XDG_CONFIG_HOME:-~/.config}/steps/mcp/<server-name>.json` (`internal/mcp/oauth.go`'s `TokenPath`/`TokenFile`) — deliberately per-user rather than colocated with a pipeline's `.steps/state.db`, and deliberately not routed through `internal/store` (see "SQLite WAL Mode" below: don't add a second `OpenStore` path or a new table for this). If you add a new secret-shaped field anywhere in this codebase, ask which of these two treatments it needs — an env-var reference folded into the hash, or a token file kept out of it entirely — rather than inventing a third.

## Project Layout

The module is a thin `main.go` entrypoint over a set of single-responsibility `internal/` packages, forming an acyclic dependency graph (each line below depends only on what's listed after `->`):

```
internal/shell, internal/template, internal/retry, internal/store, internal/config, internal/outcome   (leaves)
internal/resource, internal/workspace, internal/merkle, internal/mcp                  -> config (+ shell/template/mcp for resource; merkle -> resource too)
internal/agent                                                                        -> config, store, merkle, workspace, outcome, retry, shell, template, mcp
internal/pipeline                                                                     -> config, merkle, outcome, resource, store, workspace, agent
internal/trigger                                                                      -> config, resource, store, workspace, pipeline
main.go                                                                               -> config, store, workspace, pipeline, trigger, mcp
```

### `internal/config` — the shared data model
YAML parsing (`LoadConfig`) and every config type: `Config`, `ResourceType`, `Resource`, `Agent`, `AgentSource`, `ToolSpec`, `Task`, `FixSpec`, `Job`, `Step`, `WorkspaceConfig`. Also owns the config-merge logic that both plan-time hashing and run-time execution call, so both stay in lockstep: `Config.ResolveTask` (a task step's `run:`/`fix:`, resolved against a top-level `tasks:` entry) and `Config.ResolveAgentInvocation` (an agent step's connection/dials/tool-grant, resolved against the named `agents:` entry). Depends on nothing but the standard library and `yaml.v3`.

### `internal/pipeline` — the orchestrator
`RunJob()` walks a job's plan in order: resolves/fetches `get` steps, runs `task`/`put`/`agent` steps, and records each step's outcome. It composes every other internal package; `internal/trigger` is the only package that depends on it, and only to call `RunJob` itself once per triggered job — the single-job semantics `RunJob` implements are otherwise untouched by that feature. `hooks.go` holds the step/job hook dispatch (`runHooks`/`runOneHook`/`runHookStep`) — see [docs/control-flow.md](docs/control-flow.md).

### `internal/trigger` — cross-job downstream triggers
The cross-job counterpart to `internal/pipeline`'s single-job orchestration — see [docs/infra.md](docs/infra.md) for the full model. `Resources`/`AffectedJobs` read a `Config`'s `trigger: true` get steps; `Watch` runs two independent loops (a poller that diffs a resource's latest `check` version against `internal/store`'s `resource_checks` table and enqueues affected jobs into its `trigger_queue` table, and a worker pool that drains that queue via `pipeline.RunJob`) so a crash or a `--max-concurrent` > 1 pool doesn't lose track of pending work. `pollOnce`/`drainOne` are the unit-testable seams the loops are built from.

### `internal/agent` — agent step execution
Split by responsibility: `provider.go` (LLM client construction, persona/system-message building), `tools.go` (built-in + custom tool declarations and execution, plus the MCP pre-branch and closer bookkeeping in `buildAgentTools`), `mcptool.go` (MCP tool declarations/execution — see [docs/mcp.md](docs/mcp.md)), `conversation.go` (the tool-calling request/execute/append loop), `step.go` (`RunStep` — the exported entrypoint an agent step in the plan runs through), `fix.go` (`RunFix` — a task's `fix:` agent, built on the same conversation machinery), `openrouter.go` (OpenRouter prompt-caching request mutations — see [docs/agents.md](docs/agents.md)). Only `RunStep`/`RunFix`/`WithNewRun` are exported; everything else is package-private.

### `internal/merkle` — content-addressed planning
`Node`/`Chain`/`PlanChains` plus the content-map builders (`GetNodeContent`/`TaskNodeContent`/`PutNodeContent`/`AgentContentMap`) shared between planning and real execution, so both compute identical hashes for identical steps. Depends on `config` and `resource` (to resolve a `get` step's version the same way at plan time and run time) — nothing execution-specific.

### `internal/resource` — resource type commands
Runs a resource type's `check`/`in`/`out` shell commands (or, when `config.MCP` is set, MCP tool calls — `mcp.go`, see [docs/mcp.md](docs/mcp.md)) and selects among the versions a check returns (`ResolveVersions`, `SelectVersion`, `VersionMode`).

### `internal/mcp` — shared MCP client
Connects to a configured `mcp_servers:` entry over Streamable HTTP (`Connect`/`Client`), lists/calls its tools, and — for `auth: {type: oauth}` — handles both halves of OAuth: the interactive authorization-code + PKCE bootstrap (`login.go`'s `Login`, used only by `steps mcp login`) and the headless, silent-refreshing token source `run`/`watch` actually use (`oauth.go`'s `oauthTokenSource`, with write-back persistence on token rotation — see the Trust Boundaries note above for where the token itself lives). `internal/agent` and `internal/resource` both build on this rather than importing the vendored SDK's higher-level pieces directly. See [docs/mcp.md](docs/mcp.md).

### `internal/workspace` — per-step/per-build filesystem views
`Provider`/`BuildWorkspace`/`StepSpace` interfaces; the default shared (single-directory) implementation; the `strategy: copy` backend (`workspace_copy*.go`, portable, copy-on-write via platform-specific `cp` flags) and `strategy: btrfs` backend (`workspace_btrfs*.go`, Linux only; subvolume create/snapshot/delete, with a non-Linux stub); static `inputs:`/`outputs:` plan validation (`ValidateArtifactFlow`).

### Leaves
- **`internal/store`** — SQLite state persistence (`Store`), WAL setup, `NodeRecord` (a persistence-shape copy of `merkle.Node`, so this package doesn't need to depend on `merkle`); also owns the downstream-trigger tables (`resource_checks`, `trigger_queue`) `internal/trigger` reads/writes through `Store` methods — see [docs/infra.md](docs/infra.md)
- **`internal/shell`** — shell command execution with context, logging, output truncation; `Runner` interface (`HostRunner` via `sh -c`, `DockerRunner` via a fresh `docker run --rm` per command) selected by `NewRunner(image string)` — see [docs/infra.md](docs/infra.md)
- **`internal/template`** — YAML template rendering (e.g., `{{ .source.repo }}`) — see [docs/templating.md](docs/templating.md)
- **`internal/retry`** — linear-backoff retry loop (`retry.Do`)
- **`internal/outcome`** — classifies a step/job error into `failed`/`errored`/`aborted` for hook dispatch (`Fail` marks a task-level failure; `Classify` buckets against the job ctx) — see [docs/control-flow.md](docs/control-flow.md)

### Configuration & Examples
- **.golangci.yml** — Linter config; 40+ rules including security (gosec), correctness, concurrency, complexity checks, and a `depguard` rule per package enforcing the dependency graph above
There are four example pipelines, grouped by theme + runtime (consolidated from ten single-feature files):
- **examples/flow.yml** — **Control flow, self-verifying, modelless.** Every job is a `steps test` regression fixture verified by `assert.execution`. Covers `when:` (conditional steps — `gate-open`/`gate-closed`/`nonzero-is-false`), `to:`/`max_visits:` (bounded step transitions — `converge` revise-loop, `exhaust` self-loop firing the job hook), and hooks (`on_success`/`on_failure`/`ensure`, incl. nesting — `passing`/`failing`, whose matching asserts clear a deliberate failure). Run `steps test examples/flow.yml` or `steps run examples/flow.yml --job <name>`. No network, credentials, or model.
- **examples/agents.yml** — **Agent features (each job needs a live LLM, so it's a read-only reference, not a `steps test` fixture — no CLI mock seam).** Jobs: `review` (a custom tool with the `required:`/`max_calls:`/`args:` guards + a commented `assert.tool_calls`; needs gh), `self-heal`/`self-heal-lint` (`fix:` agent loop, simple and mapping form; needs gh), `delegate` (a `lead` agent granting a `summarizer` sub-agent via an `agent:` tool; offline but needs a local model), `mcp-tools` (a `triager` agent granted a named subset of a bearer-authenticated MCP server's tools; needs a model + a real `GITHUB_PAT`), `judge` (N-way `verdicts:` routing with a bounded revise loop over a real `draft` artifact: a `get: project` fixture feeds `writer` — which reads the project and writes `draft/summary.md`, declaring `inputs: [project]`/`outputs: [draft]` — then `critic`/`publisher`/`escalator` each declare `inputs:` for what they read; `writer` also carries `handoff: {tool: true}`, so on a revise loop it gets a `<transition_context>` block plus a `previous_run` tool over critic's recorded run — see [docs/control-flow.md](docs/control-flow.md)).
- **examples/infra.yml** — Three pipeline-infrastructure features: **downstream triggers** (`publish`/`notify`, a self-contained `counter` resource + `trigger: true` under `steps watch`; no network), an **MCP-backed downstream trigger** (`notify-linear`, the same `trigger: true` pattern but `check:` calls an oauth-configured MCP server instead of a shell command — needs a real Linear account + `steps mcp login`; see [docs/mcp.md](docs/mcp.md)), and **containerized execution** (`containerized`, `image:` on a resource_type/task/agent + a step-level override; needs docker + a local model).
- **examples/workspace.yml** — Opt-in `workspace:` isolation: `release` (a reusable task with `inputs:`/`outputs:`, an isolated agent step, and a `put` scoped to a declared input) and `release-mapped` (the same build wired with a get alias `resource:`, task `input_mapping:`/`output_mapping:`, and a `put` with `inputs: all`). On its own because `workspace:` is a pipeline-global block that would force isolation on every job in a file (needs gh + a model).

### Root Files
- **main.go** — CLI entry point; parses args into `run`/`watch`/`test`/`mcp` subcommands (`RunCmd`/`WatchCmd`/`TestCmd`/`MCPCmd`) — `run` calls `config.LoadConfig()` → `pipeline.RunJob()`; `watch` → `trigger.Watch()` (see [docs/infra.md](docs/infra.md)); `test` runs every job (force) and verifies `assert:` directives (see [docs/control-flow.md](docs/control-flow.md)); `mcp tools`/`mcp login` list a server's tools / run the interactive OAuth flow (see [docs/mcp.md](docs/mcp.md)). A global `--log-level`/`STEPS_LOG_LEVEL` flag (default `info`) controls `initLogging`'s handler level — see "Trust Boundaries Around Pipeline-Defined and Model-Directed Execution" above.
- **go.mod / go.sum** — Dependencies: yaml.v3, kong (CLI), tint (structured logging), modernc.org/sqlite, google.golang.org/genai, openai-go, golang.org/x/sys (btrfs backend), github.com/modelcontextprotocol/go-sdk (MCP client/OAuth), golang.org/x/oauth2
- **.steps/** — State cache directory (created at runtime for each pipeline's working dir). Never holds an MCP OAuth token — see the Trust Boundaries note above.

### `docs/` — feature reference
Human-friendly deep-dives for each opt-in feature area, split along the same boundaries as the `examples/*.yml` grouping above. Not force-loaded into every agent context — read the relevant one when a task touches that area:
- **[docs/agents.md](docs/agents.md)** — the agent tool-calling loop, custom tool `required:`/`max_calls:`/`args:` semantics, sub-agent delegation (`agent:` tools), top-level `tasks:` reuse.
- **[docs/control-flow.md](docs/control-flow.md)** — hooks (`on_success`/`on_failure`/`ensure`), conditional steps (`when:`), step transitions/verdict routing (`to:`/`max_visits:`/`verdicts:`), transition context on routed entry (`handoff:` — a pushed `<transition_context>` prompt block and/or a pulled `previous_run` tool), and self-verification (`assert:` + `steps test`).
- **[docs/infra.md](docs/infra.md)** — containerized execution (`image:`) and cross-job downstream triggers (`trigger: true` + `steps watch`).
- **[docs/workspace.md](docs/workspace.md)** — per-step filesystem isolation (`workspace:`, `inputs:`/`outputs:`).
- **[docs/templating.md](docs/templating.md)** — the templating functions available and the `shellquote` safety idiom.
- **[docs/mcp.md](docs/mcp.md)** — `mcp_servers:`, the three agent tool-grant forms, the resource-type `mcp:` backend, `steps mcp tools`/`steps mcp login`, and where an OAuth token is (and isn't) persisted.
- **[docs/conformance.md](docs/conformance.md)** — the living inventory of every "mirrors Concourse" claim in this codebase, which ones have a `TestConformance...` test or annotation checking them against real Concourse behavior, and how to add one.

## CI & Validation

No GitHub Actions workflows are configured. Validate before pushing via:
```bash
golangci-lint run && go test ./... && go build -v
```

## Key Implementation Details

**Every opt-in feature documented in `docs/`** (custom tool call guards, sub-delegation, workspace isolation, container execution, downstream triggers, hooks, conditional steps, step transitions, assert) follows the same two rules unless a doc says otherwise: (1) absent, its behavior and merkle hashes are byte-identical to before it existed; (2) any field it adds to a node's hashed content is folded in only when set/non-empty ("value-gated"), so a pipeline that never uses the feature hashes unaffected.

**`inputs:`/`outputs:` are optional (default empty) but flow-validated when present, `workspace:` or not.** There is no requirement to declare them — an absent `inputs:` just means the step declares nothing. But `ValidateArtifactFlow` (`internal/workspace`) now runs for **every** job, not just isolated ones: a declared `inputs:` (or an agent step's `dir:`, validated by its first path component) naming an artifact nothing earlier fetched/produced is a plan-time error even in shared mode. That's what turns "this agent was told to summarize a repo nobody fetched" into a fail-fast instead of a wasted model run. Rule (2) above still holds: inputs/outputs, `input_mapping`/`output_mapping`, and `put inputs: all` fold into a node's hash **only under a `workspace:` block** (`internal/merkle`), and get renaming's `resource:` alias folds **only when set** — so adding an `inputs:` to a shared-mode pipeline never invalidates its cache, and an unaliased get hashes byte-identically. `Step.Inputs` is a `*InputSpec` (not `[]string`) so an absent key is distinguishable from `inputs: []` (matters for `ResolveTask`'s override rule) and so put steps accept the scalar `inputs: all`. Get renaming: the artifact/step name is `step.Get`, the fetched resource is `step.GetResourceName()` (`internal/config`); `internal/trigger` polls and matches by the resolved resource name so aliases dedup.

### State Caching via Merkle Tree
- Each resource `get` and `agent` step is content-addressed (merkle tree) with its inputs (pinned versions, source config)
- After successful execution, the merkle root is stored in SQLite
- On subsequent runs, if inputs haven't changed (same versions, same configs), the step is skipped
- Use `--force` flag to bypass caching and re-run everything

## Guidance for Claude Agents

When making changes:
1. **Trust the instructions.** Verify a command only if the docs are incomplete or proven wrong in testing.
2. **Golangci-lint first.** If linting fails, the build is rejected; fix linter errors before logic changes.
3. **Run the full sequence** before committing: `go fmt ./... && go mod tidy && golangci-lint run && go test ./... && go build -v`.
4. **If touching `internal/store/store.go`**: `OpenStore` is called exactly once per process and that single handle is threaded everywhere — don't add a second `OpenStore` call path or a per-object pool, even to "help" `steps watch --max-concurrent`'s worker pool (it's already safe via `SetMaxOpenConns(1)`; see "SQLite WAL Mode" above). WAL is set via DSN pragma with no retry loop, which is only safe because of that single-open-at-startup model.
5. **Keep `internal/agent`'s conversation loop's behavior stable.** The tool-calling loop (`conversation.go`'s `runAgentConversation`) is tightly coupled to context propagation, logging, and `required:` enforcement via `tool_choice` forcing; file-organization changes within the package are fine, but refactors must preserve the loop's exact semantics — see [docs/agents.md](docs/agents.md).
6. **Respect the package dependency graph** (see Project Layout above and `.golangci.yml`'s `depguard` rules): `internal/config` depends on nothing internal; `internal/pipeline` is the only package that depends on `internal/agent`; `internal/trigger` is the only package (besides `main`) that depends on `internal/pipeline`, and only to call `RunJob` per triggered job. A new import that isn't in a package's `depguard` allow-list is a signal the change belongs in a different package, not a rule to route around.
7. **If you touch code carrying a `mirrors Concourse`/`per Concourse's model` comment**, check [docs/conformance.md](docs/conformance.md) for whether a `TestConformance...` test already covers the claim, and add one (or correct the comment) if you're changing the behavior it describes. One such claim was already found to be false — see that doc's opening paragraph.
