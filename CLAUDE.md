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
The state database (`.steps/state.db`) uses SQLite's WAL (Write-Ahead Logging) mode. **Do not** rely solely on `busy_timeout` or `MaxOpenConns(1)`—WAL requires explicit retry loops in concurrent scenarios. See `store.go` for the pattern. Recent modernc.org/sqlite versions handle this correctly; if a test flakes with "database is locked," ensure retries are in place, not just timeout config.

### Test Parallelism
Under high CPU contention (many tests in parallel), some shell/storage tests flake intermittently. Always run `go test ./... -p 1` in CI or when validating changes. Standard `go test` may pass locally but fail under load. This is **not** a bug; it's a known interaction between test timing and SQLite's internal locking.

## Project Layout

### Core Files
- **main.go** — CLI entry point; parses args, calls `run()` → `LoadConfig()` → `RunJob()`
- **config.go** — YAML parsing; defines `Config`, `ResourceType`, `Resource`, `Agent`, `Task`, `Job`
- **job.go** — Job execution; `RunJob()` orchestrates resource discovery, fetch, agent steps, and persistence
- **agent.go** — Agent step execution; LLM invocation, tool-calling loop, context management, system message building
- **store.go** — SQLite state persistence; merkle-tree-based caching to skip unchanged work
- **shell.go** — Shell command execution with context, logging, output truncation
- **resource.go** — Resource discovery and version management (check/in/out commands)
- **merkle.go** — Content-addressed caching; detects when get/agent work can be skipped
- **template.go** — YAML template rendering (e.g., `{{ .source.repo }}`)
- **retry.go** — Exponential backoff retry logic

### Configuration & Examples
- **.golangci.yml** — Linter config; 40+ rules including security (gosec), correctness, concurrency, and complexity checks
- **examples/review.yml** — Example pipeline: PR review job using an agent with `read_file`, `list_dir`, `run_shell`, and a custom `post_review` tool

### Root Files
- **go.mod / go.sum** — Dependencies: yaml.v3, kong (CLI), tint (structured logging), modernc.org/sqlite, google.golang.org/genai, openai-go
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

This resolution (`resolveTask` in `job.go`) runs identically at plan time (`merkle.go`'s `planNonGetNode`/`taskNode`) and run time (`runTaskStep`/`runTaskCommand`), so a task's merkle hash is always computed from its *resolved* `run:` string — an inline task's hash is unaffected by this feature. An undefined reference surfaces as an ordinary `FindTask` error at plan time, the same as an unknown `get`/`put`/`agent` name.

`required: true` is enforced entirely in `runAgentConversation` by tracking **success** (`exit_code == 0`), not mere invocation:
- **The model tries to stop while a required tool hasn't yet succeeded** → its next turn is constrained via the provider's `tool_choice` (`forceRequiredTool`, mapped by `genaiopenai` to OpenAI's named `tool_choice`) to a function call for that specific tool — a hard API-level constraint, not a text reminder the model could ignore.
- **A forced (or voluntary) call still fails** → that failure is appended to the conversation like any other tool result. The model gets another turn to fix it and try again — no attempt is aborted for this.
- **The safety bound**: if a provider doesn't honor `tool_choice`, or the model just can't get the required tool to succeed, `max_turns` still caps the loop and the step fails, naming the tool(s) that never succeeded.
- `withRetry`/`attempts:` (a full conversation restart from the original prompt) still exists, but only fires for *non-tool* failures — `generateOnce` erroring (LLM/transport issue) or `max_turns` exhaustion — never for a tool's own failure.
- Only custom tools can be marked `required:`; built-ins (`read_file`, `list_dir`, `run_shell`) and the fix-agent's injected task-rerun tool are never required — they're intentionally exploratory/iterative regardless.

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
5. **Check WAL constraints** if touching `store.go`: ensure retries are explicit, not just timeout config.
6. **Keep agent.go module-level.** Agent step execution is tightly coupled to context, logging, and tool-calling infrastructure; refactors must preserve the flow.
