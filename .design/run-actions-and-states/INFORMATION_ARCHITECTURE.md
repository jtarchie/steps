# Information Architecture: run actions and states

No design brief precedes this one; the visual language is inherited unchanged from `.design/nav-overflow/DESIGN_BRIEF.md` (terminal-native, one glyph per status, `.btn` + `.meta` consequence line). This document fixes **words, placement and state**, not look.

## Problem

Two families of controls say one thing and do another.

**Starting runs.** Three buttons, three labels, one route (`POST /jobs/:job/trigger`):

| Where | Label today | What it does |
|---|---|---|
| job page, primary | `▶ Trigger` | queue the job; newest unbuilt versions; cache honored |
| job page, secondary | `↻ Re-run (forced)` | same, cache ignored. Re-runs nothing. Explanation lives in a `title=` tooltip only |
| run page | `▶ Trigger new run` | identical to the first. Unrelated to the run on screen; different versions |
| follow page | heading `steps trigger X` | not a real command |
| read-only job page | "Trigger with `steps run <path> --job X`" | runs the job **locally**, not on the daemon |
| queue reasons | `manual (web)`, `manual re-run, forced (web)` | the second repeats "re-run" |

"Re-run" is Concourse's word for *same build, same versions* (`fly rerun-build`, #146). steps used it for *skip the cache*.

**Stopped states.** One word, one colour, three causes:

| State | Cause | Words today | Look | Undo |
|---|---|---|---|---|
| pipeline paused | a person | 2-line banner; pause meta says "until resumed" | red `st-failed` on overview; unpaused pipelines read "running" | `Unpause` |
| job breaker | N consecutive failures | "paused after N failures", "Out of rotation", "N consecutive" | red | `Resume` (CLI `jobs resume`), listed on the **resources** page |
| aborted run | a person | `✗ aborted` | red, same glyph as failed | — |

"paused", "Resume" and "resumed" each mean two or three things (`--resume <run>` is a third). A deliberate human pause is drawn as a failure. And a held job's Trigger is accepted, then silently dropped at claim (`runner.go` `skipIfPaused`).

## Decisions

1. **One verb for starting a run: "Trigger new run".** Concourse's "trigger new build" in steps' noun. Verb + object says a *new* run is made.
2. **"Re-run" is reserved** for #146 (same run, same versions). Nothing ships with that word until #146 does.
3. **The cache-skipping trigger stays, on the job page only**, renamed and explained on the page rather than in a tooltip.
4. **The run page keeps a trigger**, naming the job and saying the versions are not this run's. The slot beside it is reserved for #146's "Re-run with same inputs".
5. **A read-only server states it and suggests no command.** No CLI enqueues on a daemon today; any command shown would mislead.
6. **"paused" means only a human chose it** (pipeline). The breaker state is **held**, undone by **Release**. "Resume" then means only `--resume <run>`.
7. **One state vocabulary, one chip, everywhere** (below). A chip is always followed by what it stops and how to undo it.
8. **A held job's state lives with the job**: jobs list, job head, and the attention bar. Not the resources page.
9. **A manual trigger on a held job runs.** The breaker stops only automatic triggers, as the existing copy already says ("will not auto-trigger"). Today the queue row is claimed and dropped by `skipIfPaused`; that is the bug. Release stays a separate act that resets the count.
10. **A manual trigger in a paused pipeline stays refused**, a deliberate divergence from Concourse, which accepts it and leaves the build pending. The refusal is shown as a disabled button with its reason, never a 409 page.
11. **The follow page says what a queued run is waiting on**, as Concourse's "preparing build" checklist does, instead of guessing after two minutes.

## State vocabulary

Each state has exactly one word, glyph and colour, used by overview, jobs list, job head, run head, run strip and graph.

| State | Scope | Glyph + word | Colour | Why this colour |
|---|---|---|---|---|
| queued | run | `○ queued` | `--faint` | not started, not a verdict. Concourse pending is grey (was yellow) |
| running | run | `◐ running` | `--yellow` | unchanged |
| passed | run | `✓ passed` | `--green` | unchanged |
| failed | run | `✗ failed` | `--red` | the steps said no |
| errored | run | `! errored` | `--red` | the machinery broke. Concourse uses amber, but amber is steps' running; the glyph separates it instead |
| aborted | run | `■ aborted` | `--dim` | a person stopped it; not a failure. Concourse uses brown, not red. Same glyph as the Abort button |
| paused | pipeline | `⏸ paused` | `--blue` | a person chose it; Concourse's paused is blue `#4BAFF2` for pipeline and job alike |
| active | pipeline | `active` (no chip) | `--dim` | replaces "running", which collides with a run's running |
| held | job | `⊘ held` | `--red` | caused by failures and needs someone. Glyph separates it from failed. No Concourse equivalent; not called paused, so a future human job pause can take that word |

A job with no state of its own shows its latest run's chip. A held job shows `⊘ held`, then the latest run's chip.

**The chip-line pattern.** Every non-default state appears as `chip · what it stops · [Undo]` on one line, e.g.:

```
⏸ paused · no polling, no new runs; webhook deliveries wait · [Unpause]  or steps pipeline unpause -p app
⊘ held after 3 failures · won't trigger on new versions · [Release]  resets the failure count
```

Read-only: the `[Undo]` becomes the CLI command, as the paused banner does today.

## Site Map (affected pages only)

- Overview `/`: per-pipeline chip `⏸ paused` / `active`; Pause/Unpause stays
- Pipeline `/p/:p`: jobs list
  - Jobs list: new **State** column replaces **Breaker** (`⊘ held` + count). The `Pause pipeline` control stays
  - Job `/p/:p/jobs/:job/detail`: head carries state + actions
    - Follow `/p/:p/jobs/:job/follow`: the queued waiting room
  - Run `/p/:p/runs/:id`: head carries run chip + actions
  - Resources `/p/:p/resources`: **Circuit breaker section removed**
- Attention bar (every anchored page): paused item shrinks to the one-line chip pattern; `jobs ●N` links to the jobs page filtered to held jobs

## Content Hierarchy: action clusters

`.actions` in the page head, primary first, each button followed by a visible `.meta` consequence line. Tooltips carry nothing that the meta line doesn't.

### Job page head
1. State line: `⊘ held after N failures` when held
2. Actions, by state:

| Job state | Primary | Secondary |
|---|---|---|
| normal | `▶ Trigger new run` · meta *builds the newest versions it hasn't built; unchanged steps are skipped* | `▶ Trigger new run without cache` · meta *runs every step even if unchanged; still only unbuilt versions* |
| held | `Release` · meta *resets the failure count; new versions trigger it again* | `▶ Trigger new run` stays enabled · meta *runs now; held only stops automatic triggers* |
| pipeline paused | none; both triggers disabled, meta *pipeline is paused* (the attention bar carries Unpause) | — |
| read-only | *This server is read-only; runs start only from its triggers.* | — |

### Run page head
1. Run chip + two head lines (per `.design/run-page-ia`)
2. Actions, in Concourse's order: first slot `■ Abort` while running, otherwise `↻ Re-run with same inputs` (#146, empty until then); then `▶ Trigger new run of <job>` · meta *newest versions, not this run's*. Same disabled rules as the job page.
3. Concourse's shipped tooltips are the model for the meta lines: `trigger a new build` / `re-run with the same inputs` (issue #4589). steps shows them as visible text because its buttons are words, not icons.

### Follow page
1. `$ steps run <job>` heading (matches the run page), metaline `○ queued`
2. A waiting-on checklist, each line `⟳` (blocking) or `✓` (clear), modelled on Concourse's "preparing build":
   - `pipeline is not paused`
   - `serial / max_in_flight allows another run`
   - `a worker has claimed it`
   The first blocking line is the answer to "why hasn't it started?". It replaces the two-minute "Is a runner draining this queue?" guess. `held` is not a line, since a manual trigger bypasses it (decision 9).
3. `■ Abort before it starts`
3. Breadcrumb last crumb: `queued` (was `trigger`)

## User Flows

### A run failed; I want it again
1. Land on the failed run
2. See `▶ Trigger new run of build` · *newest versions, not this run's*
   - Want the same inputs → not offered until #146 (`--pin` from the CLI today)
   - Want the newest → press; the follow page shows `○ queued`, then forwards to the run
3. A step is flaky but unchanged, so the cache skips it → job page → `Trigger new run without cache`

### A job keeps failing
1. The attention bar shows `jobs ●1`; the jobs list row shows `⊘ held`
2. Open the job: `⊘ held after 3 failures · won't trigger on new versions · [Release]`
3. Push a fix, then `Trigger new run`: it runs even though the job is held
4. Release → state clears; new versions trigger it again

### I paused the pipeline and forgot
1. Every page's attention bar: `⏸ paused · no polling, no new runs; webhook deliveries wait · [Unpause]`. The pipeline switcher takes `--blue`, a quieter version of Concourse turning the whole top bar blue
2. Trigger buttons disabled with *pipeline is paused*, not a 409 error page

## Naming Conventions

| Concept | Label in UI | CLI | Notes |
|---|---|---|---|
| queue a job, cache honored | **Trigger new run** | (none on the daemon) | only verb for starting a run |
| queue a job, cache ignored | **Trigger new run without cache** | `--force` | job page only |
| same run, same versions | **Re-run with same inputs** | `fly rerun-build` analogue | reserved for #146; Concourse tooltip wording; nothing else says "re-run" |
| continue from the failed step | Resume (CLI only) | `--resume <run>` | the only meaning of "resume" |
| human stop, whole pipeline | **paused** / **Pause** / **Unpause** | `pipeline pause/unpause` | meta says "until unpaused", not "resumed" |
| unpaused pipeline | **active** | — | not "running" |
| breaker stop, one job | **held** / **Release** | `jobs release` (was `jobs resume`) | "Out of rotation" and "Breaker" column removed |
| stopped by a person | **aborted** / **Abort** | `runs abort` | neutral, not red |
| waiting to be claimed | **queued** | — | not "pending" in the UI |
| queue reasons | `trigger (web)`, `trigger, no cache (web)` | — | "manual" dropped: every web trigger is manual |

## Component Reuse Map

| Component | Used on | Behaviour differences |
|---|---|---|
| `.st` chip (+ new `st-paused`, `st-held`, `st-queued`; `st-aborted` restyled) | overview, jobs list, job head, run head, run strip, graph, queue table | none: one class per state is the point |
| chip line (`chip · stops · [Undo] meta`) | attention bar (paused), job head (held) | read-only swaps the button for the CLI command |
| `.actions` cluster | job head, run head, follow | disabled-with-reason replaces hidden-or-409 |

## Content Growth Plan

Fixed vocabulary. A new state (e.g. a human job pause, if steps adopts Concourse's) gets a row in the state table, one `st-*` class and one chip-line wording. Nothing else. #146 adds one button to a reserved slot.

## URL Strategy

- `POST /p/:p/jobs/:job/trigger` unchanged; `force=all` unchanged (the #147 tab-safety reasoning still holds)
- `POST /p/:p/jobs/:job/resume` → `/release` (pre-release, no compat alias)
- Jobs list filter for the attention link: `/p/:p?state=held`
- #146 will add `POST /p/:p/runs/:id/rerun`, keyed on a run rather than a job

## Concourse reference

Read from concourse/concourse @ d6e25a3, `web/elm/src`.

| Concourse | steps here | Match? |
|---|---|---|
| build page: `[Abort \| Rerun] [Trigger]`, tooltips `trigger a new build` / `re-run with the same inputs` (Build/Header/Header.elm) | same order, visible meta lines | yes, text instead of icons |
| no "ignore cache" verb (Concourse never skips on a content hash) | `Trigger new run without cache` | steps-only, because steps has a cache |
| paused job/pipeline: trigger accepted, build pending, "preparing build" checklist (atc/scheduler/scheduler.go:36, Build/Build.elm) | held: runs; paused pipeline: refused, disabled button; follow page checklist | diverges on purpose (decisions 9, 10) |
| pipeline and job pause both `paused`, blue; top bar turns blue; no banner (Views/Styles.elm) | `paused` blue, pipeline only; one-line attention item | yes |
| aborted brown, errored amber, pending grey (ColorValues.elm) | aborted dim, errored red + `!`, queued faint | same intent within steps' palette |
| buttons are only disabled by job config (`disable_manual_trigger`, `disable_reruns`) | disabled with a stated reason | steps adds a reason line |

## Open / out of scope

- ~~Breaker count after a passing manual run~~: verified, a pass clears it (`recordBreaker` → `RecordJobOutcome`).
- No daemon-side CLI trigger (`steps jobs trigger -p`); worth an issue if the read-only hint needs somewhere to point.
- Concourse's human **job** pause does not exist in steps; if added, it takes `⏸ paused` at job scope, which is why the breaker is not called paused.
