## Story 001 — `try:` tolerated failure

Adds a `try:` wrapper step that swallows the inner step's failure so the plan
always continues. Best-effort work like notifications, cleanup, or metrics pushes.

### What changed

**Core data model** (`internal/config/step.go`):
- `Try *Step` field on `Step`, `StepKindTry` constant, `Kind()` candidate, `kindFieldsSet()` entry
- `validateTrySteps()` — rejects bare try, try+get, try+multiple-kinds
- `visitStepTree` recurses into `step.Try`

**Validation** (`internal/config/`):
- `validateStepAssert` delegates to inner step for assert field checks
- `stepName` delegates to inner step's name (so `to:` routing targets work)
- `validateHookStep` allows try as a hook body (updated messages)
- `resolveStepReference` recurses into inner step

**Merkle hashing** (`internal/merkle/`):
- `NodeKindTry` constant, `TryNodeContent()` folds inner content under `"try"` key
- `stepContentMap()` — shared dispatcher for try/hook content building
- `tryNode()` planning function, always unskippable

**Pipeline runtime** (`internal/pipeline/`):
- `runTryStep()` dispatches inner step, swallows failure, records try node as "succeeded"
- Chain: parentHash → tryHash (succeeded) → nextStep; inner node recorded independently
- `dispatchNonGetStep`, `unskippableReason`, `executedStepName`, `stepLabel`, `resolveStepImage`, `runHookStep` all handle try

**Schema, docs, examples:**
- `steps.schema.json` — `"try"` property referencing `$defs/step`
- `docs/control-flow.md` — new "Tolerated failure" section
- `examples/try.yml` — demo pipeline

**Tests:**
- `internal/config/try_test.go` — 9 tests (acceptance, rejection, nesting, hooks)
- `internal/config/step_kind_test.go` — try and try+task cases
- `internal/merkle/merkle_test.go` — 6 tests (content wrapping, nesting, hash stability, hash changes with inner/hooks, unskippable chain)
- `e2e_test.go` — `TestEndToEndTryToleratesFailure` (try+task, plan continues, store records both nodes)

### Verification

- `go fmt ./...` ✅
- `go mod tidy` ✅
- `golangci-lint run` ✅ (0 issues)
- `go test ./...` ✅ (all passing)
- `go build -v` ✅
- Code review ✅ (PASS)
