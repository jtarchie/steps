---
name: steps-tests
description: Test and doc-corpus mechanics for the steps repo — how mutation testing guards docs/*.md, what adding a config.Step (or any config type) field costs across schema/docs/assertions/operators, the TestDocsCoverage and TestSchemaKeysMatchStructs contracts, noexec fence reasons, and the end-to-end harness (e2e/e2e_test.go, e2e/fakeprovider_test.go, newFakeLLM/newRoutedFakeLLM). Load when adding or changing a config field, writing or editing a docs/*.md example, touching mutation_test.go or dsl_mutation_test.go, or writing an end-to-end test.
---

# steps: test and doc-corpus mechanics

## Mutation testing guards the doc corpus

**It is the reason a doc example is a test rather than a decoration.** Two suites, both in `./e2e`, both included in `go test ./...` (~20s of that package's runtime; `task mutate` runs just them):

- `mutation_test.go` (`TestAssertMutation`) breaks every `assert:` field in every executed doc block, one at a time — ~215 mutants. Each must (1) still `steps validate --syntax-only` clean, so the mutant is a legal pipeline whose only defect is the expectation, (2) fail `steps test`, and (3) fail with an error naming an `assert.` field. Check (1) is what stops a mutation that broke the *load* from counting as a catch — `assert.verdict` is the live case, since naming an undeclared verdict is a load error, so that operator swaps in another *declared* verdict instead.
- `dsl_mutation_test.go` (`TestDSLMutation`) breaks the *pipeline* instead: one operator per `config.Step` yaml tag, applied at every site in the corpus until one is detected. It logs how each field was caught (`assert` = a doc assertion, `load` = validate refused it, `crash` = incidental) — a `crash` means the example asserting that field is thinner than it looks. `TestDSLMutationCoversStep` is the ratchet: every Step tag needs an operator or a written reason in `stepMutationSkips` (the reasons are load-bearing — `prompt:`/`max_context_bytes:` cannot be mutated because a *positional* fake provider answers the same however it is prompted, so asserting on them would be asserting on the fake).
- Adding a `config.Step` field therefore costs four things: a schema property, a doc example, an assertion that example can fail on, and an operator (or a reason). Adding a field to any *other* config type costs the first two — `TestDocsCoverage` and `TestSchemaKeysMatchStructs` both run over every type (`Config`/`Job`/`Step`/`Task`/`Agent`/`Resource`/`ResourceType`/`MCPServer`/`Assert`/`Defaults`/`WorkspaceConfig`), and `TestDocsCoverage` collects keys **by position**, so `config.Step`'s `tools:` is not satisfied by an `agents:` entry spelling the same word.
- `TestDocsExamplesAssert` keeps the corpus assertive: every job of every executed block must carry `assert.execution` **and** `assert.outcome`. Both, because a matching `execution:` *clears* a plan failure — with `execution:` alone a fixture passes on the broken build and the fixed one alike.
- A `noexec` fence must name why (`noexec=docker|btrfs|credentials|network|cli|stdio-mcp|approval`, `docs.NoexecReasons()`). An unexecuted example is the one that rots silently, so opting out costs a word.

## End-to-end tests live in ./e2e

`e2e/e2e_test.go`, harness in `e2e/fakeprovider_test.go`. They're the only tests that exercise CLI → config → merkle → resource → workspace → agent conversation → route → store as one pass: a pipeline YAML in a temp dir, an agent pointed at a scripted `httptest` OpenAI-compatible endpoint, and assertions at each layer (offered tools and forced `tool_choice` on the wire, tool results fed back, artifact flow into the put, verdict routing, and the `nodes`/`job_runs` rows).

They must stay in ONE package there: only `cli.Run` spans the whole stack, and `source.endpoint:` is the sole injection point (there is no injectable `model.LLM`, by design). Splitting them across sibling packages would also let `go test ./...` run them concurrently, where they contend for the docker daemon, ports and the shim binary.

Tests that reach a CLI internal no command exposes — `parseLogLevel`, `setup`, `mcpAuth`, `ConfigWatcher.check` — live beside the code in `internal/cli` instead. Paths to repo-root files (`steps.schema.json`, `docs/`, `examples/`) go through `repoFile(...)` in `e2e`, since the working directory is `e2e/`.

Reach for `newFakeLLM(t, script...)` rather than hand-rolling another `httptest` handler — or `newRoutedFakeLLM(t, fn)` when the pipeline runs agents CONCURRENTLY (`max_in_flight:`), where a positional script would be asserting on goroutine interleaving; it answers each request from its content instead.

Note that a step declaring `context: { from: ... }` opens with a *synthetic* `read_step` call-and-result pair per demanded sender (as `context_paths:` does with `read_file`), so "the history has tool traffic" is true before the model has done anything — a router deciding whether the model already acted must ask which tool was called.
