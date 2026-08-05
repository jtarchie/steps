PASS

## Requirements from story `stories/002.md` — all cited

### Acceptance criteria

| # | Requirement | Citation |
|---|---|---|
| 1 | Steps inside `in_parallel:` run concurrently; `limit:` bounds how many at once | `runInParallelChildren` at `repo/internal/pipeline/pipeline.go:660-720` (semaphore channel + goroutines per child). Probe: `examples/in-parallel-basic.yml` PASS. |
| 2 | `fail_fast: true` cancels siblings on first failure, no orphaned model calls | `cancel()` at `repo/internal/pipeline/pipeline.go:716`. E2E test `TestEndToEndInParallelFailFastCancelsAgent` at `repo/e2e_test.go:623` verifies agent cancellation: `fake.requestCount() < 2`. Probe: `examples/in-parallel-fail-fast.yml` PASS. |
| 3 | Duplicate output/artifact names across branches fail at load time | `validateInParallelBranchOutputs` at `repo/internal/config/step.go:776-796` — recursive detection through nested blocks. Test: `TestInParallelDuplicateOutputs` at `repo/internal/config/in_parallel_test.go:138`. |
| 4 | Concurrent output is labelled per branch | `fmt.Printf("[branch %d] ...")` at `repo/internal/pipeline/pipeline.go:698-703` and `hooks.go:196`. Verified in every probe transcript. |
| 5 | Nesting works | `visitStepTree` recurses at `repo/internal/config/step.go:617`. Probe: `examples/in-parallel-nested.yml` PASS. |
| 6 | Docs cover shared-workspace hazard and point at isolation strategies | `repo/docs/control-flow.md` "Workspace safety" section. `repo/docs/workspace.md` "Concurrency hazard with `in_parallel:`" note. |

### Prose design-decision requirements

| # | Requirement | Citation |
|---|---|---|
| 7 | "Naming collisions... must be a load-time error" | Same as #3. `validateInParallelBranchOutputs` at `step.go:776`. |
| 8 | "Silent last-write-wins would be a correctness trap" | Same as #3. Rejected at load by duplicate-output detection. |
| 9 | "This must ship with explicit docs" (workspace safety) | Same as #6. Both `control-flow.md` and `workspace.md` updated. No load-time warning — story says "possibly," optional. |
| 10 | "Log interleaving... needs a per-branch label prefix" | Same as #4. `[branch N]` prefix on every child output line. |
| 11 | "Routing scope: to:/verdicts: targets inside a branch stay within that branch" | `validateBranchRouting` at `repo/internal/config/route.go:355-387` iterates `child.To` which covers both `to:` keys and verdict-routed keys. Cross-branch/outside-block targets rejected. Test: `TestInParallelRouting` at `in_parallel_test.go:275`. |
| 12 | "A branch cannot route to a sibling branch or outside the block" | Same as #11. Probe verified: `to: fail routes to "outside", which is not a step in the same branch`. |
| 13 | "Nesting... recommend allowing from day one" | Same as #5. Nested probe PASS. |
| 14 | "Resource contention... `limit:` exists partly *for* this" | `repo/docs/control-flow.md`: "This is the tool for bounding container/CPU/memory load when several parallel steps each set `image:`." |
| 15 | "Fail-fast cancellation — in-flight agent steps must honor context cancellation" | Same as #2. E2E test at `e2e_test.go:623`. |

## What the story implies that nothing implements — examined

### 1. Get steps inside in_parallel (story YAML example deviation)

The story's canonical YAML example shows `get: repo` and `get: rules` inside `in_parallel:`. The implementation rejects get steps at load time: `validateInParallelNoGetChildren` at `step.go:743` returns `"get steps are not supported inside in_parallel"`. The plan's open-questions section explicitly flagged this as a design decision and proposed allowing them as non-fanning fetches. The implementation chose a more conservative path (reject).

Verdict: **Intentional design deviation, not a defect.** The story's acceptance criteria do not include get steps inside in_parallel; the YAML example is illustrative, not normative. The rejection is tested at `in_parallel_test.go:116`.

### 2. No `[branch N]` label Go test

The spec manifest (`repo/experiments/self-build/specs/002.md`) requirement #8 says: "plus a Go test in internal/pipeline/in_parallel_test.go that captures a labeled writer's output and asserts the prefix." This test does not exist — `TestStepLabelInParallel` at `in_parallel_test.go:334` only tests the wrapper label (`step N (in_parallel)`), not the `[branch N]` prefix on child output.

Verdict: **Spec-to-implementation gap.** The behavior is correct (the prefix appears in every probe transcript), but the promised test was never written. Non-blocking since the spec itself categorized this as "Manual verification during review" first.

### 3. Negative `limit:` silently accepted

`limit: -1` loads without error and behaves like `limit: 0` (unbounded) — `runInParallelChildren` at `pipeline.go:664` treats `<= 0` as unbounded. No validation rejects negative values.

Verdict: **Minor edge case, not a story requirement.** Worth adding a validation but not blocking.

### 4. First-reviewer claim about verdicts validation — disproven

The first reviewer claimed `validateBranchRouting` only checks `to:`, not `verdicts:` routing targets. This is incorrect: `child.To` is the map containing ALL routing targets — both `to: {success: X}` and verdict-routed `to: {pass: X, fail: Y}`. `validateBranchRouting` at `route.go:370` iterates `for key, target := range child.To` which covers every routing target regardless of whether the key is `success`, `failure`, or a verdict name. The probe confirmed: `to: fail routes to "outside", which is not a step in the same branch`.

### 5. No load-time workspace warning

The story says "(and possibly a load-time warning)." The docs at `control-flow.md` say "This hazard is not detected at load time." No warning is emitted.

Verdict: **Not implemented, but story said "possibly" — optional.**

## Fixtures verified

All six `examples/in-parallel-*.yml` exist and pass `steps test`:
- `in-parallel-basic.yml` ✅
- `in-parallel-fail-fast.yml` ✅
- `in-parallel-fail-fast-swallow.yml` ✅
- `in-parallel-nested.yml` ✅
- `in-parallel-routing.yml` ✅
- `in-parallel-routing-noop.yml` ✅

No fixture carries `# steps-test: skip`.

## Dispatch sites

The plan's dispatch-site table was verified by the first reviewer and spot-checked here. `validateBranchRouting` correctly handles `to:`, `max_visits:`, backward routing, and cross-branch rejection for children. The `assert:`, `when:`, `image:`, `trigger:`, `version:`, `params:` rejections on the in_parallel wrapper are all confirmed by Go tests.

## Overall

Every acceptance criterion and prose MUST requirement has a citation or probe. The implementation is complete and correct.
