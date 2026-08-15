# CLAUDE.md: steps Pipeline Runner

Trust these instructions. Only search the codebase if something here is missing or turns out to be wrong — this file is validated, not guessed.

## What this repo is

**steps** is a Go CLI that executes Concourse-style YAML pipelines (`resource_types`/`resources`/`jobs`), with an LLM `agent` step type built in alongside conventional `get`/`task`/`put` steps. Resources are discovered/fetched via templated shell commands (or MCP); `agent` steps call an LLM with tool calling. All state is cached in SQLite (WAL mode) via content-addressed (merkle) hashing, so unchanged steps are skipped on rerun.

- **No CI configured**: there is no `.github/`, no CONTRIBUTING.md. `Taskfile.yml` (go-task) is the entire validation pipeline — run it locally exactly as documented before considering any change done.

## Working rules

- **Unspecified behavior → ask "What would Concourse do?"** steps is a Concourse-shaped DSL; when a semantic isn't pinned down by this repo (what a field means at an edge, ordering, when a step is skipped, how a version is picked, what counts as a change), the default answer is whatever Concourse does. Research it thoroughly first — Concourse docs, its source, real pipeline behavior — then come back with the finding AND the open questions for the user. Do not invent a semantic and do not guess in silence; a divergence from Concourse is a deliberate decision the user makes, not a side effect of an implementation.
- **Foreign key constraints are required.** Every SQLite column in `internal/store` that references another table's key declares a `REFERENCES` constraint with an explicit `ON DELETE` action. `PRAGMA foreign_keys=ON` is set on the connection, so a missing constraint is a silently-orphaned row, not a saved line. A relationship that genuinely cannot be a foreign key needs a comment naming why.
- **TDD, outside-in.** Write the test first. Start with the happy-path end-to-end test — the one that runs a real pipeline through `run()` and asserts the outcome a user would see — and only then work downward into the narrower tests (package-level, then unit) for the branches, errors, and edge cases the e2e pass doesn't reach. An e2e that passes is the proof the feature exists; the tests below it are the proof it holds up. A feature whose first test is a unit test is a feature nobody has run.
- **Comments earn their place.** Write a comment only when it says something the code cannot: a why, a non-obvious invariant, an external constraint, a deliberate shortcut (`// ponytail:`). No restating the next line, no section banners, no narrating parameters. An extraneous comment is a defect — delete it rather than update it.

## Build, test, lint — `task` runs it all

Prerequisites: Go 1.26.6 (`go version`), golangci-lint v2.12+ (`golangci-lint version`; `brew install golangci-lint`), and go-task (`task --version`; `brew install go-task`). SQLite is vendored (`modernc.org/sqlite`, pure Go, no cgo/system SQLite needed). `task install:tools` installs the two remaining CLIs the sequence needs (`govulncheck`, `nilaway`) — neither needs network/Docker/credentials to *run* (govulncheck fetches the vuln DB, which needs network once per invocation).

Run `task` (bare — it's an alias for `task fmt`) after any code change; it runs the whole sequence below, in order, and does not require Docker or credentials:

```bash
task
```

`task fmt` runs, in order:

```bash
go fmt ./...     # ~0.3s. Auto-formats; most editors already do this.
task tidy        # go mod tidy — ~0.1s, currently a no-op — go.mod/go.sum are already
                  # tidy. If it changes anything, that's a real signal: commit the diff.
task lint        # golangci-lint run — ~2.2s. ~40 linters enabled (.golangci.yml). MUST
                  # report "0 issues" before a change is done — a failing lint blocks
                  # the build, always fix lint before touching logic further.
task kindswitch  # go run ./tools/kindswitch ./... — ~3s. Reports TAGLESS kind dispatch
                  # (`switch { case step.Put != "": }`) that omits a kind from
                  # config.Step's own Kind() table. golangci-lint's `exhaustive` covers
                  # only the TAGGED spelling (`switch kind {`) — the tagless form is
                  # `switch true`, with no enum type to reason about, so it is outside
                  # that analyzer's model, not a config gap. Must exit 0. Suppress a
                  # deliberate omission with `//kindswitch:ignore <reason>` on the line
                  # above the switch; a reason is mandatory (a bare directive is ignored
                  # and the switch keeps reporting).
task test        # go test ./... — ~11s wall (parallel, default). -p 1 (serialized,
                  # ~26s) only buys deterministic interleaved log output, not correctness
                  # (verified with -race -count=50 soak testing under CPU load,
                  # 2026-07-15: no flakes either way) — run it directly if you need that.
task build       # go build -v — ~2.5s warm cache. Produces ./steps (~56MB).
task vuln        # govulncheck ./... — ~5s (first run also fetches the vuln DB over
                  # network). Reports known CVEs actually reachable from the build —
                  # stdlib CVEs are pinned to the Go *patch* version (`go version`), not
                  # this repo's code, so a finding here is usually fixed by
                  # `brew upgrade go`, not an edit. MUST report 0 vulnerabilities.
task nilaway     # nilaway -exclude-test-files=true ./... || true — ~4s. Uber's
                  # nil-flow analyzer, advisory only — never gates a change, deliberately
                  # not added to golangci-lint's run. It has a real false-positive rate
                  # (e.g. flags slicing a possibly-nil slice, which Go allows).
```

All of them always pass clean on a fresh checkout (nilaway is advisory, so "clean" means golangci-lint/kindswitch/test/build/govulncheck are all clean — nilaway's own output is read, not gated on `task fmt`'s exit code). If any of the gating ones fails, that is a real regression — do not work around it with `--no-verify`-style shortcuts or by skipping a step.

**Every nilaway finding still gets triaged, gate or not** — "advisory" means it doesn't block a commit, not that a finding is presumed spurious. Read each one, decide real bug vs. false positive (Go's nil-slice-slicing pattern is the known FP shape), and fix the real ones — including ones a change didn't introduce. Advisory-and-ignored is how a real nil-flow bug sits in the tree indefinitely; the check exists precisely because these don't reliably show up any other way. `task nilaway` on its own is the fast way to see just this output when triaging.

**Test suite specifics:**
- Every package that spawns a goroutine (`internal/pipeline`, `internal/trigger`, `internal/web`, `internal/events`, `internal/agent`, `internal/mcp`, `internal/resource`, `internal/exprlang`, and the root package) has a `goleak_test.go` `TestMain` (`go.uber.org/goleak`) — `go test ./...` fails if a test leaves a goroutine running. One known, deliberate exception: `goleak.IgnoreTopFunction("github.com/modelcontextprotocol/go-sdk/mcp.(*streamableServerConn).Read")` in those TestMains — the go-sdk's server-side streamable-HTTP read loop does not exit when a client sends the session-termination DELETE, verified in isolation against go-sdk@v1.7.0 both with and without the client's standalone SSE stream. Not steps' leak to fix; re-check when the SDK bumps past v1.7.0. A new package that spawns a goroutine in production code needs the same `TestMain` (copy an existing `goleak_test.go`, adjust the doc comment).
- The default `go test ./...` needs no network, no Docker daemon, and no credentials. A handful of heavyweight integration tests (Docker- and btrfs-backed workspace isolation) are gated behind explicit opt-in env vars and skip cleanly otherwise: `STEPS_TEST_DOCKER=1` (pulls a real image, needs a Docker daemon) and `STEPS_TEST_BTRFS_ROOT=<dir>` (needs a real btrfs filesystem, Linux only). Don't set these unless you're specifically working on the Docker/btrfs workspace backends — they're slow and non-hermetic by design.
- `go test ./... -run TestName` runs a single test; every package's tests also pass individually.
- No test requires an LLM API key or a live model — `internal/agent`'s conversation loop is tested against a fake/mock provider seam, not a real model.
- **Doc examples are tests, guarded by mutation testing.** Adding a `config.Step` field costs a schema property, a doc example, an assertion that example can fail on, and a mutation operator (or a written reason in `stepMutationSkips`); any other config type costs the first two. **End-to-end tests live in the root package** (`e2e_test.go`, harness in `fakeprovider_test.go`) and must stay there — only `main`'s `run()` spans the whole stack. Mechanics for both: the `steps-tests` skill.

## Project layout

`main.go` is a thin kong-based CLI entrypoint over single-responsibility `internal/` packages forming an **acyclic dependency graph**, enforced by `.golangci.yml`'s `depguard` linter (each package has an explicit import allow-list — a new import outside a package's list is a signal the change belongs elsewhere, not a rule to route around).

`internal/config` carries conventions the code alone won't teach — see [internal/config/CLAUDE.md](internal/config/CLAUDE.md), which loads when you work in that package.

**Two front ends, one model.** `internal/web` serves the browser UI and `internal/events` is the stdlib-only leaf carrying run events between them — see [internal/web/CLAUDE.md](internal/web/CLAUDE.md) and [internal/events/CLAUDE.md](internal/events/CLAUDE.md), which load when you work in those packages.

**`tools/`** holds build-time checkers, not shipped code — currently just `tools/kindswitch`, the `go/analysis` pass in the validation sequence above; see [tools/kindswitch/CLAUDE.md](tools/kindswitch/CLAUDE.md).

Config files: **`.golangci.yml`** (lint rules, and the `depguard` allow-lists that ARE the dependency graph — read it there rather than from a copy that can drift) — this *is* the pre-check-in validation pipeline, there's no separate CI file.

`README.md` has the quick-start; `PRODUCT.md` is a product-framing doc, not engineering reference. `examples/` now holds only `examples/pr-review.yml` (the live-model capstone example — statically validated by `TestValidatePRReviewExample`, and EXECUTED as written by `e2e_pr_review_test.go` against a scripted provider with `gh`/`git` stubbed on PATH) and `examples/invalid/` (pipelines that must FAIL to load, each naming its error — `examples_invalid_test.go`); the rest of the runnable example corpus moved into `docs/` — see [examples/CLAUDE.md](examples/CLAUDE.md).

**`docs/*.md` is the feature-by-feature reference AND the tested example corpus.** `docs/docs.go` makes the directory a Go package (`go:embed *.md` + fenced-block extraction, stdlib-only — rendering lives in the consumers: `steps docs` uses glamour in main, the web UI's `/docs` uses goldmark in `internal/web`). Every fenced ```yaml block is a complete pipeline that `docs_test.go` (root package, same reasoning as the e2e tests) schema-validates, runs through full `steps validate`, and — unless the fence says `noexec` (validate-only) or `fragment` (prose) — executes via `run(["test", ...])`. Agent examples carry `test=<id>` in the fence info, naming a scripted fake-LLM scenario in `docs_scenarios_test.go`; the harness rewrites `source:` to the fake endpoint so the rendered YAML stays clean. Pages are indexed by [docs/README.md](docs/README.md) (`TestDocsPagesListed` keeps the index complete). Proposals are GitHub issues, not pages — `docs/` describes what steps does today.

**Any DSL change requires a docs update, and the build enforces it from several directions**: `TestDocsCoverage` asserts every yaml tag of every config type appears — *in the right position* — in at least one tested doc block (add an example to the relevant page, not an exception), `TestSchemaKeysMatchStructs` holds the same tags against `steps.schema.json` in both directions, and the mutation suites above require a `config.Step` field to be breakable. A new field lands with a schema entry AND a doc example, or the build is red.

`steps.schema.json` is the published JSON Schema for the pipeline format, hand-written and kept honest by `schema_test.go` — which asserts every example validates against it AND that each `$defs` entry's properties equal the corresponding config struct's yaml tags by reflection (`schemaDefsByType`, plus the document root against `config.Config`). **Adding a field to any of those structs therefore requires adding it to the schema**, or the build fails.
