# Design Brief: Navigation overflow — fewer hops to the run that matters

## Problem

I open the daemon's root with several pipelines loaded and nothing on the screen tells me which one is red. I pick a pipeline from the switcher, land on a jobs board, read the status column, click a job, land on a job page that is mostly history and dials, click the newest run, and then scroll a transcript looking for the step that broke. That is four hops and a scroll to reach the one thing I came for: why the latest run failed, or whether the run I just kicked off is done yet. Every hop shows me a page I did not want in order to find the link to the next one.

## Solution

Every place the UI says "that job" takes me to that job's latest run. The run page carries the job's recent history as a strip of chips, so hopping between runs of one job never leaves the transcript. The root, with several pipelines, shows each pipeline's jobs as status-colored chips, so a red job is one click from a cold tab. A failed run's header names the innermost failing step and links to it. Nothing is removed: the analytical content of the old job page moves to a job detail page reached from the strip and the breadcrumb.

From a cold tab: `/` → red chip → transcript, with the failing step named in the header. Two clicks.

## Experience Principles

1. **The transcript is the destination, not the leaf** — any link that names a job resolves to its latest run. Pages that exist to hold links to the transcript (the job page) stop being stops on the way.
2. **Red only where it earns it** — status is one glyph and one color, the same vocabulary the transcript already uses. Chips and strip entries are dim by default; a failed or running job is the only saturated thing in view. No urgency sorting that keeps an old failure on top forever.
3. **Nothing lost, only moved** — deps, dials, passed versions and the full history table keep existing, on a detail page one hop from the strip. The reader who wants them still finds them; the reader who does not never sees them.

## Aesthetic Direction

- **Philosophy**: Terminal-native. The UI already reads as a scrollback with links: `$ steps run <job>` headings, monospace at 13px, bracketed hashes, bordered panels, one glyph per status. New surfaces extend that vocabulary and add no new visual language.
- **Tone**: Calm, dense, quiet. Information density over whitespace; color as signal, never decoration.
- **Reference points**: Concourse's job page (build strip above the latest build), Concourse's dashboard (per-pipeline job boxes), a well-kept `tmux` status line.
- **Anti-references**: SaaS observability dashboards — cards with shadows, a color per widget, sparkline everywhere, KPI tiles. Anything that makes a green pipeline look busy.

## Existing Patterns

- Typography: `--mono` (ui-monospace stack) at 13px body, 12px meta, 11px small; `--sans` only for eyebrows (10.5px, 600, uppercase, tracked) and prose. `.prompt` headings prefixed with a green `$ `.
- Colors: single dark theme (`color-scheme: dark`, no light variant). `--bg/--panel/--panel2/--line/--line-soft/--code-bg` for surfaces; `--fg/--dim/--faint` for three text levels; `--green/--red/--yellow/--blue/--magenta/--cyan` for meaning. Status is `.st.st-<word>` with a glyph in `::before` (`✓ ✗ ◐ ○ –`) so color is never the only channel.
- Spacing: raw px, no scale tokens. Common values: 4, 6, 8, 10, 12, 14, 16, 20, 22. Tokens phase skipped; new CSS uses these same values.
- Components to reuse: `.pagehead` (title + actions flex), `.metaline` with `.sep`, `.eyebrow`, `.crumbs`, `.tablewrap > table`, `.st`/`.st-*`, `.hash`, `.badge`, `.dagnode` (SVG status boxes on the jobs board), `.spark`, `.deps` with `.arrow`/`.res`, `.errblock`/`.errhint`, `.btn`. Live regions: `hx-get` self + `hx-select` + `outerMorph` + `hx-select-oob` list, polled at 2500ms while visible; the transcript streams over SSE.
- Keyboard: `/` palette, `j`/`k`/`e`/`c`/`enter`/`f` on the transcript.

## Component Inventory

| Component | Status | Notes |
| --- | --- | --- |
| Run strip | New | Row of status-colored chips under the run page header: newest first, ≤20, current run marked with `aria-current`, relative time as label, id + duration in `title`. A pending queue row renders a `queued` chip linking to the follow page. Lives inside a polled htmx region so a new run appears without reload. Links to the job detail page at its end. |
| Failure lede | New (small) | One line in the run header, `failed at <step>`, `href` to the innermost failed row's anchor. Same target as the `f` key. Absent unless the run failed. |
| Deps line | Modify | The job page's `.deps` block compressed to one metaline entry on the run page: `after build [source] · feeds deploy [image]`. |
| Job chips | New | On the overview's pipeline rows: the pipeline's jobs as inline chips, name colored by latest status via `.st-*`, each linking to `/p/x/jobs/y` (→ latest run). Wraps. |
| Job detail page | Modify (rename) | The current job page minus nothing but its title: deps both ways, run history table + spark, agent dials, passed versions, trigger buttons. Served at `/p/x/jobs/y/detail`. |
| Job redirect | New (handler) | `/p/x/jobs/y` → latest run (302); queued and no run → follow page; no run at all → detail page. |
| Breadcrumb | Modify | Run page crumb for the job points at the detail page; every other job link keeps pointing at `/jobs/y`. |
| Jobs board / DAG / palette | Unchanged | Their job links already go to `/jobs/y`, which now resolves to the latest run. |

## Key Interactions

- **Click a job anywhere** → the browser lands on `/p/x/runs/<latest>`. The address bar holds the concrete run, so a copied link is stable. `/p/x/jobs/y` stays the bookmark that always means "latest".
- **Trigger from the run page** → follow page → the new run, as today. The strip on the previous run gains a `queued` then a `running` chip while you watch.
- **Click a strip chip** → that run's transcript; the strip re-renders with the new current chip marked. Chips are anchors, so middle-click opens in a tab.
- **Land on a failed run** → the header's `failed at <step>` link scrolls to and focuses the failing row (already open by default). Pressing `f` does the same.
- **Overview poll** → chip colors update in place via `outerMorph`; a job going red between polls turns red without a reload, no animation.
- **No run yet** → `/jobs/y` shows the detail page with the trigger buttons, as today's job page does.

## Responsive Behavior

- Strip chips wrap onto multiple lines below 640px; no horizontal scroll. Chip label stays the relative time; hover titles are unavailable on touch, so the current chip also carries its short id visibly.
- Overview job chips wrap inside their cell; the table keeps `.tablewrap` horizontal scroll as the last resort, as every table here does.
- The failure lede wraps with `overflow-wrap: anywhere` like the rest of the metaline.
- Nothing changes behavior on mobile, only wrapping.

## Accessibility Requirements

- Every chip is an `<a>` with visible text (the relative time or job name), never a color-only box. Status carried by the `.st-*` glyph as well as color, matching the existing rule.
- Current run chip carries `aria-current="page"`; the strip is a `<nav aria-label="Runs of <job>">`.
- Failure lede link is in the tab order before the transcript, so keyboard users reach the failing step in one Tab + Enter.
- Focus survives the 2.5s poll: chips have stable ids so `outerMorph` keeps focus (the existing nav-tabs lesson).
- Contrast: existing `--red`/`--green`/`--yellow` on `--bg` already meet AA at 13px; chips use the same text-on-background pairs, no new fills below AA.
- Redirects are 302 with the destination announced by the page title (`<job> · <status>`), as today.

## Out of Scope

- Design tokens, theming, a light mode.
- The pipeline switcher, the tabs row, the `runs` tab, the palette's ranking.
- The transcript's own rendering, folding, or streaming.
- Overview for a single-pipeline daemon (`/` already redirects).
- A CLI command that prints a run URL (no CLI trigger against the daemon exists).
- Urgency-sorted feeds or failure counts in the header (docs/web.md deliberately excludes them).
- Any change to the `/api` surface or the state schema.
