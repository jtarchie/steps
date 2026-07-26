# CLAUDE.md: steps Pipeline Runner

Trust these instructions. Only search the codebase if something here is missing or turns out to be wrong — this file is validated, not guessed.

## What this repo is

**steps** is a terminal-only Go CLI that executes Concourse-style YAML pipelines (`resource_types`/`resources`/`jobs`), with an LLM `agent` step type built in alongside conventional `get`/`task`/`put` steps. Resources are discovered/fetched via templated shell commands (or MCP); `agent` steps call an LLM with tool-calling support (`read_file`, `list_dir`, `run_shell`, custom tools, sub-agent delegation, MCP tools). All state is cached in SQLite (WAL mode) via content-addressed (merkle) hashing, so unchanged steps are skipped on rerun. `steps run` executes one job once; `steps watch` polls `trigger: true` resources and auto-runs affected jobs; `steps test` runs every job and checks `assert:` directives.

- **Language/runtime:** Go 1.26.5+ (go.mod requires 1.26.4+), single module `github.com/jtarchie/steps`, no other language toolchain needed to build or test.
- **Size:** ~16,000 lines of non-test Go source (~37,000 incl. tests), 119 `.go` files, one `main.go` entrypoint.
- **Output:** a single ~56MB static binary (`./steps`), no separate install step.
- **No CI configured**: there is no `.github/`, no Makefile, no CONTRIBUTING.md. The commands below are the entire validation pipeline — run them locally exactly as documented, in this order, before considering any change done.

## Build, test, lint — validated commands, in order

Prerequisites: Go 1.26.5 (`go version`) and golangci-lint v2.12+ (`golangci-lint version`; `brew install golangci-lint`). SQLite is vendored (`modernc.org/sqlite`, pure Go, no cgo/system SQLite needed).

Run this exact sequence after any code change — each step is fast and none require network access, Docker, or credentials:

```bash
go fmt ./...        # ~0.3s. Auto-formats; most editors already do this.
go mod tidy          # ~0.1s, currently a no-op — go.mod/go.sum are already tidy.
                      # If it changes anything, that's a real signal: commit the diff.
golangci-lint run    # ~2.2s. ~40 linters enabled (.golangci.yml). MUST report "0 issues"
                      # before a change is done — a failing lint blocks the build, always
                      # fix lint before touching logic further.
go test ./...        # ~11s wall (parallel, default), ~26s with -p 1 (serialized).
                      # Both are fine; -p 1 only buys deterministic interleaved log output,
                      # it is not required for correctness (verified with -race -count=50
                      # soak testing under CPU load, 2026-07-15: no flakes either way).
go build -v          # ~2.5s warm cache. Produces ./steps (~56MB).
```

All four always pass clean on a fresh checkout. If any of them fails, that is a real regression — do not work around it with `--no-verify`-style shortcuts or by skipping a step.

**Test suite specifics:**
- The default `go test ./...` needs no network, no Docker daemon, and no credentials. A handful of heavyweight integration tests (Docker- and btrfs-backed workspace isolation) are gated behind explicit opt-in env vars and skip cleanly otherwise: `STEPS_TEST_DOCKER=1` (pulls a real image, needs a Docker daemon) and `STEPS_TEST_BTRFS_ROOT=<dir>` (needs a real btrfs filesystem, Linux only). Don't set these unless you're specifically working on the Docker/btrfs workspace backends — they're slow and non-hermetic by design.
- `go test ./... -run TestName` runs a single test; every package's tests also pass individually.
- No test requires an LLM API key or a live model — `internal/agent`'s conversation loop is tested against a fake/mock provider seam, not a real model.

## Project layout

`main.go` is a thin CLI entrypoint (kong-based, `run`/`watch`/`test`/`mcp` subcommands) over single-responsibility `internal/` packages forming an **acyclic dependency graph**, enforced by `.golangci.yml`'s `depguard` linter (each package has an explicit import allow-list — a new import outside a package's list is a signal the change belongs elsewhere, not a rule to route around):

```
internal/shell, internal/template, internal/retry, internal/store, internal/config, internal/outcome   (leaves)
internal/resource, internal/workspace, internal/merkle, internal/mcp                  -> config (+ shell/template/mcp for resource; merkle -> resource too; mcp -> shell for HostEnv)
internal/agent                                                                        -> config, store, merkle, workspace, outcome, retry, shell, template, mcp
internal/pipeline                                                                     -> config, merkle, outcome, resource, store, workspace, agent
internal/trigger                                                                      -> config, resource, store, workspace, pipeline
main.go                                                                               -> config, store, workspace, pipeline, trigger, mcp
```

One-liner per package:

- **`internal/config`** — YAML parsing (`LoadConfig`) and every config type (`Config`, `Resource`, `Agent`, `Task`, `Job`, `Step`, ...); also the config-merge logic (`ResolveTask`, `ResolveAgentInvocation`) both plan-time hashing and run-time execution share, plus `run_file:`/`system_file:`/`prompt_file:`/`file:` include resolution. Depends on nothing internal.
- **`internal/pipeline`** — the orchestrator; `RunJob()` walks a job's plan (get/task/put/agent) and records outcomes. The *only* package that depends on `internal/agent`.
- **`internal/trigger`** — cross-job downstream triggers (`steps watch`); the only package besides `main` that depends on `internal/pipeline`.
- **`internal/agent`** — agent step execution: `provider.go` (LLM client/persona), `tools.go` (built-in + custom tool exec), `mcptool.go` (MCP tool exec), `conversation.go` (the tool-calling loop), `step.go` (`RunStep`, the exported entrypoint), `fix.go` (`RunFix`). Only `RunStep`/`RunFix`/`WithNewRun` are exported.
- **`internal/merkle`** — content-addressed planning (`Node`/`Chain`/`PlanChains`), shared content-map builders so planning and execution hash identically.
- **`internal/resource`** — resource type `check`/`in`/`out` execution (shell or MCP-backed).
- **`internal/mcp`** — shared MCP client (Streamable HTTP or stdio), OAuth bootstrap/refresh.
- **`internal/workspace`** — per-step/per-build filesystem views; default shared mode plus opt-in `strategy: copy`/`strategy: btrfs` isolation backends.
- **`internal/store`** — SQLite persistence (`Store`), WAL setup, downstream-trigger tables.
- **`internal/shell`** — command execution (`HostRunner`/`DockerRunner`), output capture/truncation.
- **`internal/template`**, **`internal/retry`**, **`internal/outcome`** — YAML templating, linear-backoff retry, step/job error classification.

Config files: **`.golangci.yml`** (lint rules + the `depguard` dependency graph above) — this *is* the pre-check-in validation pipeline, there's no separate CI file. **`go.mod`** pins the Go version and every dependency.

`README.md` has the quick-start; `PRODUCT.md` is a product-framing doc, not engineering reference. `examples/*.yml` are runnable, self-contained example pipelines (several self-verifying via `steps test`) — one per feature area: `flow.yml` (conditionals/hooks/transitions), `attempts.yml` (retry/timeout), `agents.yml` (tool guards, sub-delegation, MCP grants, verdict routing — needs a live model), `infra.yml` (triggers, containers), `workspace.yml` (isolation). `docs/*.md` is the feature-by-feature reference, not force-loaded into every task — read the one relevant to what you're touching: [agents.md](docs/agents.md), [control-flow.md](docs/control-flow.md), [infra.md](docs/infra.md), [workspace.md](docs/workspace.md), [templating.md](docs/templating.md), [mcp.md](docs/mcp.md), [conformance.md](docs/conformance.md).
