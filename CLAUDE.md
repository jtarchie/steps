# CLAUDE.md: steps Pipeline Runner

Trust these instructions. Only search the codebase if something here is missing or turns out to be wrong — this file is validated, not guessed.

## What this repo is

**steps** is a Go CLI that executes Concourse-style YAML pipelines (`resource_types`/`resources`/`jobs`), with an LLM `agent` step type built in alongside conventional `get`/`task`/`put` steps. Resources are discovered/fetched via templated shell commands (or MCP); `agent` steps call an LLM with tool-calling support (`read_file`, `list_dir`, `run_shell`, custom tools, sub-agent delegation, MCP tools). All state is cached in SQLite (WAL mode) via content-addressed (merkle) hashing, so unchanged steps are skipped on rerun. `steps run` executes one job once; `steps watch` polls `trigger: true` resources and auto-runs affected jobs; `steps test` runs every job and checks `assert:` directives; `steps web` serves a local browser UI over that same state.

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
go run ./tools/kindswitch ./...
                      # ~3s. Reports TAGLESS kind dispatch (`switch { case step.Put != "": }`)
                      # that omits a kind from config.Step's own Kind() table. golangci-lint's
                      # `exhaustive` covers only the TAGGED spelling (`switch kind {`) — the
                      # tagless form is `switch true`, with no enum type to reason about, so it
                      # is outside that analyzer's model, not a config gap. Must exit 0.
                      # Suppress a deliberate omission with `//kindswitch:ignore <reason>` on
                      # the line above the switch; a reason is mandatory (a bare directive is
                      # ignored and the switch keeps reporting).
go test ./...        # ~11s wall (parallel, default), ~26s with -p 1 (serialized).
                      # Both are fine; -p 1 only buys deterministic interleaved log output,
                      # it is not required for correctness (verified with -race -count=50
                      # soak testing under CPU load, 2026-07-15: no flakes either way).
go build -v          # ~2.5s warm cache. Produces ./steps (~56MB).
```

All of them always pass clean on a fresh checkout. If any of them fails, that is a real regression — do not work around it with `--no-verify`-style shortcuts or by skipping a step.

**Test suite specifics:**
- The default `go test ./...` needs no network, no Docker daemon, and no credentials. A handful of heavyweight integration tests (Docker- and btrfs-backed workspace isolation) are gated behind explicit opt-in env vars and skip cleanly otherwise: `STEPS_TEST_DOCKER=1` (pulls a real image, needs a Docker daemon) and `STEPS_TEST_BTRFS_ROOT=<dir>` (needs a real btrfs filesystem, Linux only). Don't set these unless you're specifically working on the Docker/btrfs workspace backends — they're slow and non-hermetic by design.
- `go test ./... -run TestName` runs a single test; every package's tests also pass individually.
- No test requires an LLM API key or a live model — `internal/agent`'s conversation loop is tested against a fake/mock provider seam, not a real model.
- **End-to-end tests live in the root package** (`e2e_test.go`, harness in `fakeprovider_test.go`). They're the only tests that exercise CLI → config → merkle → resource → workspace → agent conversation → route → store as one pass: a pipeline YAML in a temp dir, an agent pointed at a scripted `httptest` OpenAI-compatible endpoint, and assertions at each layer (offered tools and forced `tool_choice` on the wire, tool results fed back, artifact flow into the put, verdict routing, and the `nodes`/`job_runs` rows). They must stay in the root package: only `main`'s `run()` spans the whole stack, and `source.endpoint:` is the sole injection point (there is no injectable `model.LLM`, by design). Reach for `newFakeLLM(t, script...)` rather than hand-rolling another `httptest` handler — or `newRoutedFakeLLM(t, fn)` when the pipeline runs agents CONCURRENTLY (`max_in_flight:`), where a positional script would be asserting on goroutine interleaving; it answers each request from its content instead. Note that a step declaring `context: { from: ... }` opens with a *synthetic* `read_step` call-and-result pair per demanded sender (as `context_paths:` does with `read_file`), so "the history has tool traffic" is true before the model has done anything — a router deciding whether the model already acted must ask which tool was called.

## Project layout

`main.go` is a thin kong-based CLI entrypoint over single-responsibility `internal/` packages forming an **acyclic dependency graph**, enforced by `.golangci.yml`'s `depguard` linter (each package has an explicit import allow-list — a new import outside a package's list is a signal the change belongs elsewhere, not a rule to route around).

`internal/config` carries conventions the code alone won't teach — see [internal/config/CLAUDE.md](internal/config/CLAUDE.md), which loads when you work in that package.

**Two front ends, one model.** `internal/web` serves the browser UI and `internal/events` is the stdlib-only leaf carrying run events between them — see [internal/web/CLAUDE.md](internal/web/CLAUDE.md) and [internal/events/CLAUDE.md](internal/events/CLAUDE.md), which load when you work in those packages.

**`tools/`** holds build-time checkers, not shipped code — currently just `tools/kindswitch`, the `go/analysis` pass in the validation sequence above; see [tools/kindswitch/CLAUDE.md](tools/kindswitch/CLAUDE.md).

Config files: **`.golangci.yml`** (lint rules, and the `depguard` allow-lists that ARE the dependency graph — read it there rather than from a copy that can drift) — this *is* the pre-check-in validation pipeline, there's no separate CI file.

`README.md` has the quick-start; `PRODUCT.md` is a product-framing doc, not engineering reference. `examples/*.yml` are runnable, self-contained example pipelines — see [examples/CLAUDE.md](examples/CLAUDE.md), which loads when you work in that directory.

`docs/*.md` is the feature-by-feature reference, indexed by [docs/README.md](docs/README.md) — not force-loaded into every task, so read the one relevant to what you're touching.

`steps.schema.json` is the published JSON Schema for the pipeline format, hand-written and kept honest by `schema_test.go` — which asserts every example validates against it AND that its step properties equal `config.Step`'s yaml tags by reflection. **Adding a field to `config.Step` therefore requires adding it to the schema**, or the build fails.
