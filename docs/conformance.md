# Concourse conformance

`steps` makes specific, scattered claims that it "mirrors Concourse" or matches "Concourse's model" — in code comments (`internal/config`, `internal/resource`, `internal/pipeline`) and in docs. Until 2026-07-23, none of these claims had ever actually been checked against real Concourse behavior. One of them was wrong: `internal/config/config.go`'s `Step.Version` doc said `version: every` "mirrors Concourse's get.version field," but `internal/pipeline/pipeline.go`'s `runGetStep` aborted its entire version fan-out on the first version whose build failed — real Concourse's version-selection cursor (`atc/db/versions_db.go`'s `NextEveryVersion`) advances regardless of build status. The bug was found by accident, not by any check; this doc and the tests it describes exist so the next divergence is found on purpose.

## Out of scope (decided, don't re-propose)

- **No vendoring/importing Concourse's Go code.** `github.com/concourse/concourse` is Apache-2.0 and Go, so license isn't the blocker — its version-selection/`passed:` logic (`atc/scheduler/algorithm`) is hard-coupled to Postgres via `atc/db.VersionsDB` (a concrete struct, hand-written SQL, not an interface). Confirmed impractical: even Concourse's *own* unit tests for this logic spin up a real Postgres instance (`atc/scheduler/algorithm/suite_test.go`).
- **No full Concourse (docker-compose: ATC+worker+Postgres) in CI.** No lightweight/in-memory distribution exists upstream. Disproportionate to a project that deliberately implements a *subset* of Concourse's model, not the whole thing.
- Instead: **hand-transcribed characterization tests**, each citing exactly which Concourse doc page or source location (at a stated version/ref) it was transcribed from.

## Claim inventory

Living checklist — update when a claim is added, resolved, or found to diverge.

| Claim | Location | Status |
|---|---|---|
| `get.version`: `latest`/`every`/`pinned`; `every` fans out per version, continuing past a failed one | `config.go`'s `Step.Version`, `resource.go`'s `VersionMode` | Conformance test: `TestConformanceGetVersionEveryContinuesPastFailure` (`internal/pipeline/conformance_test.go`) |
| `check` returns versions oldest→newest, latest last; steps doesn't re-sort | `resource.go`'s `CheckVersions` | Conformance-annotated: `TestSelectVersion`/"latest when unpinned" (`internal/resource/resource_test.go`) |
| `check`/`in`/`out` JSON stdin/stdout contract | `resource.go` | Conformance-annotated (`CheckVersions`/`RunOut` doc comments); gap-filled: `TestConformanceRunOutUnparsableStdoutIsNilNotError` |
| `get: resource:` aliasing shares version history by the real resource name | `config.go`'s `Step.Resource` | Conformance-annotated: `TestResourcesAndAffectedJobsResolveGetAlias` (`internal/trigger/trigger_test.go`) |
| Task `input_mapping`/`output_mapping` rebind only the external plan-artifact name | `config.go`'s `InputMapping`/`OutputMapping` | Conformance-annotated: `TestRunJobIsolatedGetAliasMappingAndPutAll` (`workspace_integration_test.go`) |
| Five hook modifiers (`on_success`/`on_failure`/`on_error`/`on_abort`/`ensure`) with Concourse's exact firing conditions | `hooks.go` | Conformance-annotated: `TestRunHooksRouting`, `TestRunHooksAbortGracePeriod` (`internal/pipeline/hooks_test.go`) |
| A job that never got a workspace fires no hooks | `pipeline.go` (`RunJob`) | **Not a Concourse claim** — Concourse has no literal job-level hook construct to compare against; comment corrected to state this as steps's own design choice, not parity |
| `docs/infra.md`'s `passed:` note, `docs/workspace.md`'s `detect`-default note | docs | Already-honest, self-documented divergences — precedent for how to handle a real one, not something to test |

## How to add a conformance test

1. Find the specific claim: grep this repo for `mirrors Concourse`/`per Concourse`/`Concourse's` — a conformance test traces to one of those, not to a general belief about how Concourse works.
2. Pin a Concourse reference: a doc page (concourse-ci.org/docs/...) or, if the behavior isn't documented, a source location at a release tag (e.g. `@ v8.2.4` — check the latest release first). Prefer Concourse's own docs or test suite (`atc/scheduler/algorithm/algorithm_test.go`'s Ginkgo `Entry(...)` scenarios are precise, dozens of them) over reading implementation source directly; when you do have to read source, say so in the citation and treat it as a finding, not an official guarantee.
3. Transcribe the *scenario*, not the implementation — a `steps` fixture (Go-constructed `config.Config`, or a YAML pipeline written to a temp file and run via `run()`/`mustRun()`, matching whichever pattern the surrounding test file already uses) reproducing the same input/output shape the Concourse doc/test describes, scaled to what `steps` actually models.
4. Name it `TestConformance...` (or, if annotating a pre-existing test that already covers the behavior, add a `// TestConformance note:` comment to it instead of duplicating) so `go test -run TestConformance ./...` finds the whole set.
5. Cite: the Concourse doc URL/section, or source file + symbol + pinned ref; and which of steps's own "mirrors Concourse" comments it verifies.
6. If it fails against current `steps` behavior, that's a real divergence — fix the divergence, not the test, unless it's a deliberate, already-documented one (see the `docs/infra.md`/`docs/workspace.md` precedent above), in which case document the divergence next to the claim it partially contradicts instead of leaving the claim overbroad.

## Known gaps

- No fixture yet for `passed:` or `serial`/`serial_groups:` — both are proposed, not yet implemented (`docs/proposals/stories/003-passed-cross-job-fan-in.md`, `004-serial-groups.md`). Add their conformance tests as part of implementing them, not before.
- `input_mapping`/`output_mapping`'s two undocumented sub-behaviors (an omitted key defaults to binding by its own declared name; a mapping naming a nonexistent plan artifact is a load-time error) aren't in Concourse's docs — they're `steps`'s own reasonable design choices, not Concourse claims, and don't need a conformance test (there's nothing upstream to conform to).
