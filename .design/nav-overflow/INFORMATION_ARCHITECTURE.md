# Information Architecture: steps web — navigation overflow

Scope: the path from a cold tab to a run's failing step, and the pages on it. Everything under `/p/:pipeline` not listed as changed keeps its current shape (see docs/web.md "What it shows").

## Site Map

- Root `/`
  - one pipeline served → 302 to `/p/:pipeline` (unchanged)
  - several → overview: pipelines table **+ job chips per row** (changed), recent-runs feed (unchanged)
- Pipeline `/p/:pipeline` — jobs board, list + DAG (unchanged; its job links now resolve to a run)
  - Job `/p/:pipeline/jobs/:job` — **redirect** (changed):
    - latest run exists → 302 `/p/:pipeline/runs/:id`
    - no run, queue row pending for this job → 302 `/p/:pipeline/jobs/:job/follow?since=<enqueued_at ms>`
    - no run, nothing queued → 302 `/p/:pipeline/jobs/:job/detail`
    - Job detail `/p/:pipeline/jobs/:job/detail` — **new address for today's job page** (deps, history table + spark, dials, passed versions, trigger buttons)
    - Follow `/p/:pipeline/jobs/:job/follow?since=` (unchanged)
    - Latest-run probe `/p/:pipeline/jobs/:job/latest-run?since=` (unchanged, JSON for follow)
  - Runs `/p/:pipeline/runs` (unchanged)
    - Run `/p/:pipeline/runs/:run` — transcript **+ run strip + failure lede + deps line** (changed)
      - Events stream `…/events` (unchanged)
  - Nodes, config, approvals, questions, mcp, resources, search (unchanged)
- Docs `/docs` (unchanged)

A "latest run" is the newest by `started_at`, running included — `LatestRunByJob` already answers it. A run reaped by retention makes a copied `/runs/:id` a 404; `/jobs/:job` is the durable address and re-resolves on every visit.

## Navigation Model

- **Primary navigation**: the header tabs (jobs · runs · resources · [mcp] · approvals · questions · docs) and the pipeline switcher. Unchanged. Maximum stays seven.
- **Secondary navigation**:
  - Overview → job chips: the pipeline's jobs inline in its row, config order, each `→ /jobs/:job`.
  - Jobs board → job name, DAG node, latest-run status: all `→ /jobs/:job` (the status cell may keep its direct `/runs/:id`, same destination today).
  - Run page → run strip: this job's recent runs as chips, newest first, current marked, `queued` chip for a pending trigger, trailing link `→ /jobs/:job/detail`.
  - Run page → failure lede: `failed at <step>` `→ #<step anchor>`.
  - Breadcrumb on run, follow, node pages: `jobs / <job> / …` where `<job>` `→ /jobs/:job/detail` — the one job link that does NOT resolve to a run, because from inside a run the strip already shows latest and the crumb is how the reader reaches what the strip does not carry.
- **Utility navigation**: `/` palette (unchanged; its job hits go through the redirect), attention list under the header (unchanged).
- **Mobile navigation**: unchanged. Chips and the strip wrap; nothing collapses into a menu.

Depth: cold tab → transcript is two clicks with several pipelines, one with one pipeline. Transcript → failing step is zero clicks (open by default) or one (lede link).

## Content Hierarchy

### Overview `/` (several pipelines)
1. Pipelines table, one row per pipeline: name, waiting badge, **job chips**, last run, queued, state, file — the chips are the new first-glance answer to "which job is red, in which pipeline". Chips replace nothing; the `Jobs` count column becomes the chips cell.
2. Recent runs feed across pipelines — unchanged, still time-ordered.

### Run `/p/:pipeline/runs/:run`
1. Page head: `$ steps run <job> #<id>`, status glyph, started, duration, config sha, workspace; actions (abort / trigger) — unchanged.
2. **Failure lede** (failed runs only): one metaline entry `failed at <innermost failed step>` linking to its row. Sits in the metaline so a live run that fails gains it on the done-reload.
3. **Deps line**: `after <up> [res] · feeds <down> [res]` as a metaline entry, faint. Absent when the job has neither.
4. **Run strip**: `<nav aria-label="Runs of <job>">`, chips newest-first, ≤20, then `all runs ↗` to `/jobs/:job/detail`. Polled region.
5. Job error / notes / diff notes / spend / machines — unchanged.
6. Transcript — unchanged.

### Job detail `/p/:pipeline/jobs/:job/detail`
Today's job page, in today's order: head + trigger actions, depends on / feeds, run history + spark (live region), agent dials, passed versions. Title stays `$ steps job <job>`; crumb `jobs / <job> / detail`.

### Jobs board `/p/:pipeline`
Unchanged content. The `Latest run` column keeps its direct run link; the `Job` column and DAG nodes keep `/jobs/:job`, which now lands on the same run.

## User Flows

### Cold tab, several pipelines, "why is it red"
1. Open `/`. See pipelines table; one row has a red chip.
2. Click the chip → `/jobs/:job` → 302 → `/runs/<latest>`.
3. Header metaline says `✗ failed · … · failed at build/test`. Failed rows are open. Click the lede (or press `f`) → the failing row, output visible.
   - If the latest run is running → the header says `◐ running`; the transcript streams; the strip shows the previous outcome one chip to the right.

### "Is the thing I triggered done"
1. From a run page or the detail page, click Trigger → `follow?since=now` → the run once it starts (unchanged).
2. From anywhere else, click the job → `/jobs/:job`:
   - a run has started → the running run's transcript, streaming.
   - queued, not started → follow page with `since=<enqueued_at>`; it forwards when the run exists.
   - nothing → detail page with the trigger button.
3. From an older run of the same job, the strip's newest chip reads `queued` (→ follow) then `◐ 3s ago` (→ the run) within one poll.

### Compare with the previous run
1. On a run page, the strip shows neighbours; the current chip is marked.
2. Click a neighbour → its transcript; the strip re-marks. Middle-click opens a tab.

### Reach dials / passed versions
1. On a run page, click the crumb `<job>` or the strip's trailing `all runs` → `/jobs/:job/detail`.

## Naming Conventions

| Concept | Label in UI | Notes |
|---|---|---|
| Newest run of a job, running included | latest run | Already the board's column header. Never "last build". |
| The row of a job's recent runs | runs of `<job>` | The nav's aria-label; visually unlabeled. |
| One entry in the strip / one job on the overview | chip | Internal name only; the UI shows the time or the job name. |
| Pending queue row | queued | Matches the queue table's status word and the follow page. |
| The step that broke | failed at `<step>` | Innermost, same target as the `f` key; the docs already say "the step that actually broke". |
| The old job page | detail | Route segment and crumb label. |
| Upstream / downstream | after `<job>` / feeds `<job>` | Compressed from the detail page's "Depends on" / "Feeds". |

## Component Reuse Map

| Component | Used on | Behavior differences |
|---|---|---|
| `.st.st-<word>` status glyph+color | chips (overview, strip), lede | Chips add a label (job name / relative time) after the glyph; no new colors. |
| Polled live region (`hx-get` self + `hx-select` + `outerMorph` + `hx-select-oob`) | overview (existing region gains chips), run strip (new region `#run-strip`), detail (today's `#job-runs`) | The run page gets its first polled region; the transcript keeps SSE. `#run-strip` names `#statusline` and `#attention` oob like the others. |
| `.metaline` + `.sep` | run head (lede, deps line) | Entries are conditional; separators only between present entries. |
| `.crumbs` | run, follow, node, detail | Job crumb → `/detail`. |
| `.deps` with `.arrow` / `.res` | detail page | Unchanged; the run page uses the compressed one-line form instead. |
| `.tablewrap` | overview, detail | Chips cell wraps; table keeps horizontal scroll as last resort. |
| Redirect handler pattern (`c.Redirect(302, …)`) | `/jobs/:job` | Three-way branch; `handleIndex` is the precedent. |

## Content Growth Plan

- **Runs grow**, bounded by `run_history:`. The strip shows ≤20; the detail page's history table shows `historyLimit`; the runs tab is the unbounded-ish cross-job view. No pagination added.
- **Jobs grow slowly** (config-bound). Overview chips wrap; a pipeline with 40 jobs makes a tall row, acceptable. Past that, the jobs board is one click away and the chips cell could cap with `+N` — not built now.
- **Pipelines grow** with the daemon; the table already scales by row.
- The palette remains the search surface for anything not visible.

## URL Strategy

- Pattern: `/p/:pipeline/<collection>/<name>[/<facet>]`. Facets of a job: `detail`, `follow`, `latest-run`. A run's only facet is `events`.
- Dynamic segments: pipeline slug, job name, run id (hash), node hash, config sha — unchanged.
- Query parameters: `since=<unix ms>` on `follow` and `latest-run` (existing); `after=<seq>` on `events` (existing); `q=` on `search` (existing). None added.
- Redirect codes: `/jobs/:job` uses **302** (a bookmark that must re-resolve every visit; 301 would let a browser cache the run id). Trigger keeps **303**.
- Stability: `/jobs/:job` is the address to share for "the job"; `/runs/:id` is the address to share for "this run". A 404 on a shared run id means retention reaped it, and the page already says so.
