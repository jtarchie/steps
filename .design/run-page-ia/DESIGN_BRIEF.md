# Design Brief: Run page — the transcript above the fold

## Problem

I open a failed pr-review run and the transcript is not on the screen. Above it, in order: a header line that wraps twice (status, the failing step, when, how long, config sha, workspace, what fed it, what it feeds), the run strip, a screen of red because the job error is the whole wrapped message, up to three Δ lines about what changed since the last pass, a spend table with one row per agent step, sometimes a machines table, and a keyboard hint. Every one of those is true and useful, and every one of them is in the way. On a pr-review run the spend table alone is ten rows. I scroll past a page of chrome to reach the first step, and the step that broke — the one the header already named — is the one thing I actually came to read. The red block up top and the step below it are the same error twice, except the step's copy is suppressed so the top one can exist.

## Solution

The head answers one question, in two lines: what happened, and where. The transcript follows within the first screen. Everything that was above it either becomes a pointer (one metaline entry that links down) or moves to where it is read: the job error into the failing step's row, the per-step spend and machines tables below the transcript with anchors, the three Δ notes folded into one dim line under the strip, the keyboard hint onto the transcript's own top edge. Nothing is deleted; the audit reader who wants ceilings, cache rates and filesystems scrolls or clicks `#spend`, and gets exactly the table they have today.

A failed run, first screen: `$ steps run review #BXFGSW` · `✗ failed · failed at review/verify · 4m12s · 2h ago` · one fainter provenance line · the strip · a Δ line · the transcript, with the failing row open and carrying its error.

## Experience Principles

1. **The transcript is the page; the rest is its margin** — anything that is not a step earns a place above the transcript only by being one line long. A table is never above the fold. This resolves "useful" against "in the way": useful things get a pointer up top and a home below.
2. **Say it where it is read, once** — an error is read on the step that raised it, a token count on the step that spent it, drift on the run that drifted. The head names and links; it does not repeat. `DistinctError` inverts: the step is the home, the head defers.
3. **Same page, live or done** — the layout does not rearrange when a run finishes. A running run shows the same two head lines with a `so far` spend, the same anchors, the same tables (partial) below. The closing reload redraws numbers, not structure.

## Aesthetic Direction

Inherited from `.design/nav-overflow/DESIGN_BRIEF.md`; nothing here adds visual vocabulary.

- **Philosophy**: Terminal-native. Scrollback with links; `$ steps run` headings; one glyph per status; bordered panels.
- **Tone**: Calm, dense, quiet. The only saturated thing in the first screen is the status glyph and the failing row's `✗`.
- **Reference points**: Concourse's build page (header, then the plan, immediately); a `git log -1 --stat` where the summary is two lines and the detail is below.
- **Anti-references**: CI pages that open on a summary card, a cost widget and a "metadata" table before the log; anything that makes a passed run's page start with numbers.

## Existing Patterns

- Typography: `--mono` 13px body, 12px metaline, 11px small; `--sans` eyebrows. `.prompt` headings.
- Colors: single dark theme. `--fg/--dim/--faint` text levels; `--red/--green/--yellow` for status; `.errblock` uses its own dark-red surface (`#191110`, border `#4a2c29`).
- Spacing: raw px. `.pagehead` margin-bottom 20; `.metaline` 12px, `.sep` 8px gutters; `.runstrip` margin `-8px 0 18px`; `.diffnote` `-8px 0 18px`; `.spend` `1rem 0`; `.callout` margin-top 22.
- Components to reuse: `.pagehead`, `.metaline` + `.sep`, `.runstrip`, `.diffnote` (`Δ` prefix, `.chg` yellow), `.errblock`/`.errhint`, `.log .err` inside a step body, `.spend`/`.spendhead`/`.spendtable`/`.spendscroll`, `.keyhint`, `.callout`, `.transcript`, `.step`/`.stephead`/`.stepbody`. Live regions: `#run-strip` polled, the transcript over SSE, nothing else changes live.
- Anchors: `.step { scroll-margin-top: 3.2rem }` and `:target` highlight already exist for `#step` links; the new `#spend`/`#machines` anchors reuse that offset.

## Component Inventory

| Component | Status | Notes |
| --- | --- | --- |
| Outcome line | Modify | `.metaline` line 1: status glyph+word, `failed at <step>` link, duration, started. In that order: outcome, place, cost in time, when. |
| Provenance line | New (small) | Second `.metaline`, `--faint`: `config [sha]` · workspace · `after …` · `feeds …` · `spend 412k tokens · 61% cached · $1.20 → #spend` · `3 placed → #machines`. Each entry conditional, separators only between present entries. `so far` suffix on spend while running. |
| Job error block | Modify | Rendered above the strip ONLY when no failed step's error is contained in it (placement, MCP, resource failures before any step). Otherwise absent. `.errhint` follows it. |
| Step error | Modify | The innermost failing step's body carries its full error (`DistinctError` inverted: the step is the home). The MCP hint renders under it when the step is the one blamed. |
| Δ line | Modify | One `.diffnote`: `Δ vs last passed [sha]: config changed [sha] · build, test changed content` — or `no step's content moved` — composed from `ComparedConfig` and `Changed`. `replayed from [sha]` stays its own `.diffnote` line above it. |
| Spend section | Move | Same markup, placed after the transcript (and after the `Folded = cached` callout), with `id="spend"` and `scroll-margin-top`. |
| Machines section | Move | Same markup, after spend, `id="machines"`. |
| Transcript head | New (small) | A flex row above `.transcript` holding the `.keyhint` right-aligned; empty left side. Hidden on `hover: none` as today. |
| Run strip, run notes, replayed line, callout | Unchanged | Positions unchanged relative to the head. |

## Key Interactions

- **Land on a failed run** → line 1 names the step; the failing row is open (as today) and now shows the full error text in its body. The `f` key and the lede link land on the same row and the error is there.
- **Click `#spend` from the provenance line** → the page scrolls to the spend table under the transcript, `:target` highlight on the section. Back button returns. A pasted `…/runs/<id>#spend` opens on the table.
- **Run is running** → line 1 shows `◐ running · 1m03s`; the provenance line's spend entry reads `… so far`; `#spend` and `#machines` link to whatever rows exist. Closing reload replaces numbers in place; nothing moves.
- **Run failed before any step** (image pull, placement, MCP probe) → the `.errblock` renders above the strip exactly as today; there is no step to hold it.
- **Poll** → `#run-strip` keeps polling; the two metalines are not live regions today and stay so (their live content — duration — already updates via `#run-duration` on the done reload). The `liveregion_test.go` probe decides whether the new provenance entries count as changing state; expected: spend changes only on the done reload, same as the tables today.

## Responsive Behavior

- Both metalines wrap with `overflow-wrap: anywhere`, as today. Line 2 wraps first and reads fine broken, because every entry is self-labelled.
- The transcript head's key hint is already `display: none` on `hover: none`; the flex row collapses to zero height when empty.
- Spend and machines keep `.spendscroll` horizontal scroll (min-width 480 / 720) — below the transcript this no longer competes with the rows for the first screen at 375px.
- Nothing changes behavior on mobile, only position.

## Accessibility Requirements

- Line 1 is the first thing after the `h1` in DOM order; the `failed at` link is the first focusable element after the actions, before the strip.
- `#spend` and `#machines` anchors are `<section>`s with an accessible name: the existing `.spendhead .lbl` becomes the section's heading (`aria-labelledby`), so a screen reader's landmark list names them.
- The step body's error keeps `.log .err` (red on panel, AA at 13px) and is inside the row the `f` key focuses, so a keyboard reader reaches the error text in one keypress.
- Provenance line is `--faint` on `--bg`: verify it still meets AA at 12px; if not, use `--dim` for the text and `--faint` only for separators.
- Skip-to-transcript: not added; with the head at two lines plus the strip, the transcript is within one screen and within ~10 tab stops.

## Out of Scope

- The strip, the transcript's rows, folding, streaming, or the `step` template's head.
- A new route or facet (`/runs/:id/spend`); anchors only.
- Per-step spend on step rows (rejected in the grill: loses the rollup).
- Collapsible/remembered state for the tables.
- The job detail page, jobs board, overview.
- Any change to what is recorded, the `/api` surface, or the store.
- Tokens, theming, light mode.
