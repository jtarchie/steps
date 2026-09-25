# Information Architecture: steps web — the run page

Scope: one page, `/p/:pipeline/runs/:run`, and where each thing on it lives. Routes, the strip, the transcript's rows and the stream are unchanged (see `.design/nav-overflow/INFORMATION_ARCHITECTURE.md` for the path INTO this page).

## Site Map

- Run `/p/:pipeline/runs/:run` — **changed**: the order of what is on it
  - `#run-strip` — this job's other runs (unchanged)
  - `#transcript` — the steps (unchanged rows; the failing row gains its error)
  - `#spend` — **new anchor** on the spend section, now below the transcript
  - `#machines` — **new anchor** on the machines section, now below the transcript
  - `#<step anchor>` — every step, as today
  - `…/events` — stream (unchanged)
- Everything else under `/p/:pipeline` — unchanged

No new route. A run has one address and its parts are fragments of it.

## Navigation Model

- **Primary / utility navigation**: unchanged (tabs, switcher, palette, attention list).
- **Secondary navigation, within the page** — the only thing this IA changes:
  - Outcome line → `failed at <step>` → `#<step anchor>` (exists).
  - Provenance line → `spend …` → `#spend`; `N placed` → `#machines` (new, in-page).
  - Provenance line → `config [sha]` → config page; `after`/`feeds` → job (exist).
  - Δ line → `[sha]` → the compared run; `[sha]` → its config (exist).
  - Strip → sibling runs; `all runs` → detail (exist).
  - Step row → `#` anchor, hash → node page (exist).
  - Key hint: `f` → innermost failure (exists; same row as the lede).
- **Depth**: from landing to the failing step's error text is zero clicks (row is open by default) and zero scrolls on a typical run. From landing to the spend table is one click (`#spend`) or one scroll.
- **Mobile**: nothing collapses; the two metalines wrap, the hint hides on touch, the tables keep their own horizontal scroll below the transcript.

Ranked by how often a reader needs it, which is the order the page now follows: outcome → failing step → the steps → cost → drift → machines.

## Content Hierarchy

### Run `/p/:pipeline/runs/:run`

1. **Page head** — `h1` `$ steps run <job> #<id>`; actions (abort / trigger) at right. Unchanged.
2. **Outcome line** (`.metaline`, first) — `<status>` · `failed at <step>` (failed runs with a failed step) · `<duration>` · `<started>`. Answers "what happened, where, how long ago" in one glance; the only red text on the first screen besides the failing row.
3. **Provenance line** (`.metaline`, second, faint) — `config [sha]` · `<workspace>` · `after <job> [res]` · `feeds <job> [res]` · `spend <tokens> · <cached>% · <cost|unpriced>[ · N truncated][ so far] → #spend` · `<N> placed → #machines`. Each entry conditional; separators only between present entries. This is the index of everything the page holds below the transcript.
4. **Run notes** (`.runnotes`) — job-level warnings. Rare; when present, they are a warning and stay high. Unchanged.
5. **Job error block** (`.errblock` + `.errhint`) — **only** when the run has an error and no failed step's error is contained in it (failed before any step: image pull, placement, MCP probe, resource check). Otherwise absent from the head entirely.
6. **Run strip** (`#run-strip`) — unchanged.
7. **Replayed line** (`.diffnote`) — `replayed from [sha] — …`. Provenance of the run itself; stays a line of its own. Unchanged text.
8. **Δ line** (`.diffnote`, one) — `Δ vs last passed [sha]: config changed [sha] · build, test changed content` / `… · no step's content moved` / `… : build changed content — everything else replayed from cache`. Composed from `ComparedTo`, `ComparedConfig`, `Changed`; absent when there is no prior success. Replaces the two separate config/changed lines.
9. **Transcript head** — a flex row above `#transcript` carrying the `.keyhint` right-aligned. No heading text; the transcript is the page and does not label itself.
10. **Transcript** (`#transcript`) — rows unchanged, except: the innermost failing step's body carries the run's error text when the job error contains it (the head no longer does). MCP hint renders under that error when the blamed server matches.
11. **Folded = cached callout** — unchanged, directly after the transcript.
12. **Spend section** (`<section id="spend">`) — same head line and table as today. Below the fold by design.
13. **Machines section** (`<section id="machines">`) — same as today, after spend.

What is NOT on the page any more: nothing. Every element above has a home.

## User Flows

### Failed run, "why is it red"
1. Land on `/runs/:id` (from a chip, the strip, the runs tab, or a pasted link).
2. First screen: outcome line says `✗ failed · failed at review/verify · 4m12s · 2h ago`; the strip; the Δ line; the transcript with the failing row open and its error text in the body.
3. Reader reads the error where the step is. Presses `f` or clicks the lede if the row is off-screen.
   - Error text is the plan's wrapping of the step's error → shown on the step only.
   - Run errored before any step → `.errblock` above the strip, transcript says `This run recorded no steps.`, no lede (no failed step to name).
4. "Did that cost anything?" → provenance line: `spend 412k tokens · 61% cached · $1.20 → #spend`. Click → the table, `:target`-highlighted, same columns as today.

### Live run, "is it done yet"
1. Land while running: `◐ running · 1m03s`; provenance line's spend reads `… so far` when any step has reported; `#spend`/`#machines` present when their sections are.
2. Transcript streams; the reader is at the bottom within one screen of the top.
3. `done` → closing reload: numbers change, sections fill in; nothing above the transcript grows except the Δ line (computed only once the run is compared) and the `failed at` entry.

### Audit, "what did each step spend / where did it run"
1. Land on any run. Scroll past the transcript, or click `→ #spend` / `→ #machines`.
2. Tables are exactly today's. `steps runs where -p <pipeline>` remains the terminal equivalent, as docs/web.md says.

### Pasted `…/runs/:id#spend`
1. Page opens scrolled to the spend section, `scroll-margin-top` clearing the sticky header, section highlighted.

## Naming Conventions

| Concept | Label in UI | Notes |
|---|---|---|
| Line 1 of the head | outcome line | Internal; unlabeled in the UI. |
| Line 2 of the head | provenance line | Internal; unlabeled. Faint. |
| Link down to a section | `→ #spend`, `→ #machines` | The arrow says "on this page, below"; a bare `spend` would read as the tab it is not. |
| Cost so far on a live run | `so far` | Suffix on the spend entry only while `Running`. Never `partial` (that word is taken by `$0.42+3?`). |
| Steps the merkle key moved on | `changed content` | Unchanged wording from today's diffnote. |
| The step that broke | `failed at <step>` | Unchanged. |
| Error with no step to hold it | job error | Unchanged label in `.errblock`. |
| Spend section heading | `spend` | The existing `.spendhead .lbl`; becomes the section's accessible name. |
| Machines section heading | `machines` | Same. |

## Component Reuse Map

| Component | Used on | Behavior differences |
|---|---|---|
| `.metaline` + `.sep` | head, twice | Second instance is faint and carries in-page links; first instance loses config/workspace/deps. |
| `.errblock` + `.errhint` | head (no-step failures only) | Same markup; new render condition. |
| `.log .err` in `.stepbody` | failing step | Existing element; the error now renders here for the common case. `DistinctError` becomes "step's error, or the run's when this is the innermost failure and the run's contains it". |
| `.diffnote` | replayed line, Δ line | Δ line composes what two lines said. |
| `.keyhint` | transcript head | Same element, new parent (a flex row, `justify-content: flex-end`). |
| `.spend` / `.spendhead` / `.spendtable` | `#spend`, `#machines` | Same markup; `<section>` gains `id` + `aria-labelledby` + `scroll-margin-top`. |
| `.callout` | after transcript | Unchanged. |
| `#run-strip` polled region | head | Unchanged; the two metalines stay outside any live region (their content changes only on the done reload, as the tables do today). `liveregion_test.go` is the arbiter. |

## Content Growth Plan

- **Agent steps grow per pipeline** (pr-review: ~10). The spend table grows one row each; below the transcript that costs the reader nothing until they ask for it. No cap, no pagination.
- **Placements grow** with placed steps; same treatment.
- **Error text grows** with the failure; it now lives in a step body that is already `pre-wrap` and folds with the row, so a 40-line error costs the first screen nothing when the reader folds it.
- **Provenance entries grow only by feature** (a new kind of section gets a new `→ #anchor` entry). Rule: an entry is one short phrase and a link; a number needs a unit; nothing on line 2 is a sentence.

## URL Strategy

- Pattern: unchanged, `/p/:pipeline/runs/:run`.
- Fragments: `#transcript`, `#run-strip`, `#spend`, `#machines`, `#<step anchor>`. Section ids are lowercase words matching their `.lbl`; they must not collide with a step anchor (`step-…` prefix) — `spend` and `machines` do not.
- Query parameters: none added.
- Redirects: none.
- Stability: `#spend` on a run with no spend section lands at the top of the page silently, as any missing fragment does; the provenance line never links to an anchor that is not rendered.
