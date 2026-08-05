## Summary

Adds `in_parallel:` — a new step kind that runs multiple child steps concurrently with an optional `limit:` (max in-flight) and `fail_fast:` (cancel siblings on first failure). This is the keystone feature that `across:`, `ensemble:`, and `race:` will build on.

Each child runs its own hooks, records its own outcome, and participates in artifact flow. The wrapper aggregates outcomes and is always unskippable. Duplicate output names across branches are rejected at load. Cross-branch routing is rejected at load. Concurrent output is labelled `[branch N]`.

## Files changed

- **`internal/config/step.go`** — New `InParallelSpec` type, `StepKindInParallel`, `Kind()`/`kindFieldsSet()`/`visitStepTree()` extensions, `validateInParallelSteps()`, `validateInParallelNoGetChildren()`, `validateInParallelBranchOutputs()`, `collectInParallelOutputs()`, `resolveInParallelReferences()`
- **`internal/config/route.go`** — `stepName()` returns `""` for in_parallel; new `validateInParallelRouting()`, `validateInParallelRoutingStep()`, `buildBranchPos()`, `validateBranchRouting()`
- **`internal/config/artifact.go`** — Refactored `validateStepArtifactDecls()` into helpers; in_parallel case delegates to `validateInParallelBranchOutputs()`
- **`internal/config/assert.go`** — `rejectAssertInsideInParallel()` rejects assert on wrapper; `validateStepAssert()` gets `StepKindInParallel` case
- **`internal/config/guard.go`** — Rejects `when:` on in_parallel wrapper
- **`internal/config/image.go`** — Rejects `image:` on in_parallel wrapper
- **`internal/config/handoff.go`** — `validateHandoffSegment()` recurses into in_parallel children via `checkInParallelHandoff()`
- **`internal/config/handoffnote.go`** — `validateHandoffNoteSegment()` recurses into in_parallel children via `validateHandoffNoteInParallel()`; each branch is its own chain
- **`internal/config/hooks.go`** — Allows `StepKindInParallel` in hooks; error message updated
- **`internal/config/config.go`** — Adds `c.validateInParallelSteps` to validator list
- **`internal/pipeline/pipeline.go`** — `dispatchNonGetStep()` dispatches to `runInParallelStep()`; new `runInParallelStep()`, `runInParallelChildren()`; handoff is nil for all children
- **`internal/pipeline/hooks.go`** — `runHookStep()` dispatches to `runInParallelHookStep()`; `executedStepName()` returns `""` for in_parallel; `stepLabel()` returns `"step N (in_parallel)"`
- **`internal/pipeline/execlog.go`** — `recordExecution()` tolerates nil log; `recordStepExecution()` skips in_parallel wrapper; new `execLogRealKey`/`execLogCtx()` for nested recording
- **`internal/pipeline/route.go`** — `unskippableReason()` returns `"in_parallel step"`
- **`internal/pipeline/guard.go`** — `resolveStepImage()` returns `""` for in_parallel
- **`internal/merkle/merkle.go`** — `NodeKindInParallel`, `InParallelNodeContent()`, `inParallelNode()`; dispatch in `hookContentMap()`/`stepContentMap()`/`planNonGetNode()`
- **`internal/workspace/workspace.go`** — Refactored `validateStepArtifactFlow()`; in_parallel case delegates to `validateInParallelArtifactFlow()`
- **`steps.schema.json`** — `in_parallel` property with `limit`, `fail_fast`, `steps`
- **`docs/control-flow.md`** — New `## Concurrency (in_parallel:)` section covering syntax, limits, fail-fast, routing scope, nesting, hooks, output labeling, workspace safety
- **`docs/workspace.md`** — Concurrency hazard note with cross-reference to control-flow.md
- **`docs/README.md`** — Updated table to include `in_parallel:`
- **`e2e_test.go`** — `TestEndToEndInParallelFailFastCancelsAgent` verifies context cancellation propagates to agent tool execution

## New test files

- **`internal/config/in_parallel_test.go`** — 15 subtests: kind detection, empty/missing steps, ambiguous kinds, nested acceptance, hook acceptance, get rejection, duplicate outputs (flat and nested), trigger/image/when rejection, routing (within-branch accepted, cross-branch rejected, outside-block rejected, backward-without-max_visits rejected, backward-with-max_visits accepted)
- **`internal/pipeline/in_parallel_test.go`** — 9 tests: all-succeed, one-fails, fail-fast, limit serialization, nested execution, unskippable reason, step label, executed step name, resolve step image
- **`internal/pipeline/hooks_test.go`** — `TestRunInParallelHookFailFast`

## New examples (6 fixtures, all pass `steps test`)

- `examples/in-parallel-basic.yml` — two tasks with `limit: 2`
- `examples/in-parallel-fail-fast.yml` — `fail_fast: false` tolerates failing child
- `examples/in-parallel-fail-fast-swallow.yml` — regression: fail_fast: false does NOT swallow
- `examples/in-parallel-nested.yml` — two-level nesting
- `examples/in-parallel-routing.yml` — within-branch routing validated but not followed
- `examples/in-parallel-routing-noop.yml` — pins routing-is-noop behavior

## Verification

- `go fmt ./...` ✅
- `go mod tidy` ✅
- `golangci-lint run` ✅ (0 issues)
- `go test ./...` ✅
- `go build -v` ✅
- Code review: PASS
