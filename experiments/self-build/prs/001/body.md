## Story 001 — `try:` tolerated failure

Adds a `try:` wrapper step so a **task-level** failure of the step it wraps
doesn't stop the plan. Best-effort work like notifications, cleanup, or
metrics pushes.

The wrapper is **transparent**: the only thing it changes is whether the plan
walker stops. Everything that *observes* the outcome — the wrapped step's own
hooks, the wrapper's hooks, a `to:` route on the wrapper, the recorded node —
sees the real result. And only `outcome.Failed` is tolerated: an
infrastructure error or a Ctrl-C still stops the run, the same line `to:`
routing already draws.

### What changed

**Core data model** (`internal/config/step.go`):
- `Try *Step` field on `Step`, `StepKindTry` constant, `Kind()` candidate, `kindFieldsSet()` entry
- `Step.Unwrap()` / `unwrapStep()` — the single place "what does this plan step actually run" is answered
- `validateTrySteps()` — rejects bare try, try+get, try+multiple-kinds, and routing fields written one level too deep
- `visitStepTree` recurses into `step.Try`

**Validation** (`internal/config/`):
- Field division: `to:`/`max_visits:` belong on the wrapper (the only step with a position in the plan) and are rejected on the step it wraps; `verdicts:`/`handoff:`/`handoff_note:` stay on the `agent:` step being wrapped, which is what the agent runtime reads — so a tolerated agent still routes on its verdict and still receives its transition context
- `assert:` is rejected anywhere inside a `try:`, wrapper and wrapped step alike: `try:` swallows exactly the failure an assert reports, so it could only ever sit in a `steps test` suite reporting PASS on the fixture it was written to catch
- `stepName` delegates to the wrapped step's name (so `to:` routing targets work)
- `validateHookStep` allows try as a hook body (updated messages)
- `resolveStepReference` recurses into the wrapped step

**Merkle hashing** (`internal/merkle/`):
- `NodeKindTry` constant, `TryNodeContent()` folds inner content under `"try"` key
- `stepContentMap()` — shared dispatcher for try/hook content building
- `tryNode()` planning function, always unskippable, named after the step it wraps

**Pipeline runtime** (`internal/pipeline/`):
- `runTryStep()` runs the wrapped step through `runNonGetStep` — so its `when:` guard, its hooks and its execution-log entry all behave normally — and returns its real disposition, verdict and error
- `tolerateTryFailure()` is the wrapper's whole effect, applied **after** routing in `executeNonGetStep`: that ordering is what keeps `to: {failure: ...}` reachable, and `toleratedByTry` is what keeps an aborted or infra-errored step from being reported as a green job
- Same toleration for a `try:` hook body, which otherwise still failed its green step via `runHooks`' promotion of a failed `on_success`/`ensure`
- `recordStepExecution()` — the wrapper records nothing of its own, so one execution isn't double-counted in `assert.execution` and a guard-skipped inner step isn't counted at all
- `handoffFor` unwraps, so a tolerated agent is entered with its carry rather than nil
- `dispatchNonGetStep`, `unskippableReason`, `executedStepName`, `stepLabel`, `resolveStepImage` all handle try

**Artifacts** (`internal/workspace/`):
- `validateStepArtifactFlow` handles try, so a wrapped producer's `outputs:` reach later steps instead of making the whole job statically unrunnable

**Schema, docs, examples:**
- `steps.schema.json` — `"try"` property referencing `$defs/step`
- `docs/control-flow.md` — "Tolerated failure" section
- `examples/try.yml` — five modelless, self-verifying jobs (`steps test examples/try.yml`)

**Tests:**
- `internal/config/try_test.go` — acceptance, rejection, nesting, hooks, field placement, agent-field transparency, `Unwrap`
- `internal/config/step_kind_test.go` — try and try+task cases
- `internal/merkle/merkle_test.go` — content wrapping, nesting, hash stability, hash changes with inner/hooks, unskippable chain
- `internal/pipeline/try_test.go` — toleration classification, hook-body toleration (plus its unwrapped control), execution-log de-duplication, handoff unwrapping
- `internal/workspace/artifact_flow_test.go` — outputs published through a wrapper, wrapped step's own bad input still caught
- `e2e_test.go` — plan continues past a tolerated failure; `to: {failure:}` on the wrapper fires and the wrapped step's hooks see the real outcome; an infra error is **not** tolerated
- `assert_test.go` — `steps test examples/try.yml`

### Verification

- `go fmt ./...` ✅
- `go mod tidy` ✅
- `golangci-lint run` ✅ (0 issues)
- `go test ./...` ✅ (all passing)
- `go build -v` ✅
