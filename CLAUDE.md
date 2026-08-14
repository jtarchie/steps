# CLAUDE.md: steps Pipeline Runner

Trust these instructions. Only search the codebase if something here is missing or turns out to be wrong — this file is validated, not guessed.

## What this repo is

**steps** is a Go CLI that executes Concourse-style YAML pipelines (`resource_types`/`resources`/`jobs`), with an LLM `agent` step type built in alongside conventional `get`/`task`/`put` steps. Resources are discovered/fetched via templated shell commands (or MCP); `agent` steps call an LLM with tool-calling support (`read_file`, `list_dir`, `run_shell`, custom tools, sub-agent delegation, MCP tools). All state is cached in SQLite (WAL mode) via content-addressed (merkle) hashing, so unchanged steps are skipped on rerun. `steps run` executes one job once; `steps watch` polls `trigger: true` resources and auto-runs affected jobs; `steps test` runs every job and checks `assert:` directives; `steps web` serves a local browser UI over that same state; `steps docs` renders the embedded documentation in the terminal.

- **No CI configured**: there is no `.github/`, no Makefile, no CONTRIBUTING.md. The commands below are the entire validation pipeline — run them locally exactly as documented, in this order, before considering any change done.

## Build, test, lint — validated commands, in order

Prerequisites: Go 1.26.6 (`go version`) and golangci-lint v2.12+ (`golangci-lint version`; `brew install golangci-lint`). SQLite is vendored (`modernc.org/sqlite`, pure Go, no cgo/system SQLite needed). Two more tools join the sequence below, both `go install`-able and neither needing network/Docker/credentials to *run* (govulncheck fetches the vuln DB, which needs network once per invocation): `go install golang.org/x/vuln/cmd/govulncheck@latest` and `go install go.uber.org/nilaway/cmd/nilaway@latest`.

Run this exact sequence after any code change — each step is fast and none require Docker or credentials (govulncheck needs network to fetch the vuln DB, cached locally after the first run):

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
govulncheck ./...    # ~5s (first run also fetches the vuln DB over network). Reports known
                      # CVEs actually reachable from the build — stdlib CVEs are pinned to
                      # the Go *patch* version (`go version`), not this repo's code, so a
                      # finding here is usually fixed by `brew upgrade go`, not an edit.
                      # MUST report 0 vulnerabilities before a change is done.
nilaway -exclude-test-files=true ./... || true
                      # ~4s. Uber's nil-flow analyzer, advisory only — never gates a change,
                      # deliberately not added to golangci-lint's run. It has a real false-
                      # positive rate (e.g. flags slicing a possibly-nil slice, which Go
                      # allows), so triage each finding rather than chasing zero.
```

All of them always pass clean on a fresh checkout (nilaway is advisory, so "clean" means golangci-lint/kindswitch/test/build/govulncheck are all clean — nilaway's own output is read, not gated on). If any of the gating ones fails, that is a real regression — do not work around it with `--no-verify`-style shortcuts or by skipping a step.

**Test suite specifics:**
- Every package that spawns a goroutine (`internal/pipeline`, `internal/trigger`, `internal/web`, `internal/events`, `internal/agent`, `internal/mcp`, and the root package) has a `goleak_test.go` `TestMain` (`go.uber.org/goleak`) — `go test ./...` fails if a test leaves a goroutine running. One known, deliberate exception: `goleak.IgnoreTopFunction("github.com/modelcontextprotocol/go-sdk/mcp.(*streamableServerConn).Read")` in those TestMains — the go-sdk's server-side streamable-HTTP read loop does not exit when a client sends the session-termination DELETE, verified in isolation against go-sdk@v1.7.0 both with and without the client's standalone SSE stream. Not steps' leak to fix; re-check when the SDK bumps past v1.7.0. A new package that spawns a goroutine in production code needs the same `TestMain` (copy an existing `goleak_test.go`, adjust the doc comment).
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

`README.md` has the quick-start; `PRODUCT.md` is a product-framing doc, not engineering reference. `examples/` now holds only `examples/pr-review.yml` (the live-model capstone example — statically validated by `TestValidatePRReviewExample`, shape-pinned by `e2e_pr_review_test.go`) and `examples/invalid/` (pipelines that must FAIL to load, each naming its error — `examples_invalid_test.go`); the rest of the runnable example corpus moved into `docs/` — see [examples/CLAUDE.md](examples/CLAUDE.md).

**`docs/*.md` is the feature-by-feature reference AND the tested example corpus.** `docs/docs.go` makes the directory a Go package (`go:embed *.md` + fenced-block extraction, stdlib-only — rendering lives in the consumers: `steps docs` uses glamour in main, the web UI's `/docs` uses goldmark in `internal/web`). Every fenced ```yaml block is a complete pipeline that `docs_test.go` (root package, same reasoning as the e2e tests) schema-validates, runs through full `steps validate`, and — unless the fence says `noexec` (validate-only) or `fragment` (prose) — executes via `run(["test", ...])`. Agent examples carry `test=<id>` in the fence info, naming a scripted fake-LLM scenario in `docs_scenarios_test.go`; the harness rewrites `source:` to the fake endpoint so the rendered YAML stays clean. Pages are indexed by [docs/README.md](docs/README.md) (`TestDocsPagesListed` keeps the index complete). Proposals are GitHub issues, not pages — `docs/` describes what steps does today.

**Any DSL change requires a docs update, and the build enforces it twice**: `TestDocsCoverage` asserts every `config.Step` yaml tag appears in at least one tested doc block (add an example to the relevant page, not an exception), and `schema_test.go` asserts the same tags against `steps.schema.json`. A new `config.Step` field therefore lands with a schema entry AND a doc example, or the build is red.

`steps.schema.json` is the published JSON Schema for the pipeline format, hand-written and kept honest by `schema_test.go` — which asserts every example validates against it AND that its step properties equal `config.Step`'s yaml tags by reflection. **Adding a field to `config.Step` therefore requires adding it to the schema**, or the build fails.
