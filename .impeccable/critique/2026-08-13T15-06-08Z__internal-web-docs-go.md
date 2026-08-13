---
target: web /docs + steps docs CLI rendering
total_score: 24
max_score: 40
na_heuristics: 
p0_count: 1
p1_count: 2
timestamp: 2026-08-13T15-06-08Z
slug: internal-web-docs-go
---
# Critique: steps docs surfaces (web /docs + steps docs CLI)

Method: dual-agent (A: design review · B: detector + browser evidence)

## Design Health Score

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 2 | Web docsnav marks the current page (green, `aria-current`); CLI has zero position awareness — an 848-line page dumps with no pager, no page name in output |
| 2 | Match System / Real World | 4 | IA speaks the adopter's language ("Writing pipelines" vs "Reference", a LENGTH column) |
| 3 | User Control and Freedom | 1 | On /docs the app tabs render `href="/p/"`, `/p//approvals` — all 404; the `/` palette fetches `/p//search`. The way back into the app is dead |
| 4 | Consistency and Standards | 2 | Code blocks are stock gruvbox (`#282828` cool gray, keys in bright red) inside a warm-black ANSI-16 world; CLI headings are glamour's yellow-on-purple pills |
| 5 | Error Prevention | 1 | 5 cross-page anchors silently land at page top (goldmark hyphenates `_` in ids; the markdown uses GitHub-style slugs); glamour tables silently drop words |
| 6 | Recognition Rather Than Recall | 2 | Alphabetical docsnav fights README's curated order (agents-internals before agents); no in-page TOC on a 14,040px-tall page |
| 7 | Flexibility and Efficiency | 3 | curl-readable HTML, raw-markdown piping, `.md`-optional arg — but `WithWordWrap(100)` shears in an 80-col terminal |
| 8 | Aesthetic and Minimalist Design | 3 | Prose typography is authored and calm; dragged down by red code walls (web) and purple pills (CLI) |
| 9 | Error Recovery | 2 | CLI 404 lists every valid page but leaks Go internals ("(see Pages)"); web 404 lists nothing |
| 10 | Help and Documentation | 4 | The corpus itself is exemplary — every example CI-executed and labeled |
| **Total** | | **24/40** | **Functional, not yet credible as the product's own reading room** |

## Design Specificity Verdict

**Web: authored frame, off-the-shelf heart.** The prose layer is a real member of the "annotated transcript" system — mono headings over sans body, 880px measure, hairline h2 rules, `--panel2` code chips, green `aria-current` in the rail. But ~80% of a doc page's pixel area is code blocks, and those are stock chroma gruvbox: cool-gray `#282828` panels on the warm `#111310` ground, every YAML key in bright red `#fb4934`. In a UI whose own manifesto says red means what it means in a terminal, every example pipeline reads at a squint like failure output.

**CLI: delegated, not designed.** `glamour.WithAutoStyle()` + `WithWordWrap(100)` and nothing else — H1s as yellow-on-purple 256-color pills, mauve YAML keys. The piped-raw-markdown path and `.md`-optional argument are product-shaped; the styled path is factory settings on the surface the product's POV calls home.

**Deterministic scan:** static template scan clean (0 findings across all 10 templates). In-page detector found 30–132 findings per page: line-length (~123–135 chars/line in the 880px sans measure — the detector caught what the review had praised; the measure is wide for 15px sans), tiny text (11.96px table cells; 11–11.7px code), and per-page "single font / flat hierarchy" banners that are largely false positives against the deliberate two-voice compact system. conformance.md at 390px renders a ~2428px-wide canvas (6.2× overflow); agents.md 873px vs 390 (2.2×), 2 of 3 tables overflowing an `overflow-x: visible` article.

**Visual overlays:** headless browser — no user-visible overlay tab exists; findings above are the console/DOM fallback (cloakbrowser surfaces no Playwright console events; detect.js execution was verified via DOM overlay counts).

## Overall Impression

The bones are excellent — a tested corpus, clean semantic HTML, deliberate degradation paths (curl, pipes) — and the prose typography extends the app's world convincingly. But the two things a reader spends the most time on (code blocks on the web, everything on the CLI) got no design attention, the mobile experience is broken by unwrapped tables, and the single navigation route from docs back into the app 404s. The biggest opportunity: make the code blocks and the CLI wear the product's own palette — the design system already exists, it just stops at the fence line.

## What's Working

1. **The corpus-as-tests contract, surfaced in the copy.** "Every YAML example is extracted and executed by the test suite" is the most trust-building sentence a pipeline runner's docs can open with — and it's true, and the fences' test markers never leak into either rendering.
2. **Prose typography is authored.** Mono-headed/sans-bodied hierarchy, hairline rules, code chips — /docs looks like the product, not an iframed docs site.
3. **Degradation is deliberate.** Server-side rendering keeps /docs curl-readable; `stdoutIsTerminal()` gives pipes raw markdown; routes keep the `.md` suffix so relative links resolve unrewritten.

## Priority Issues

- **[P0] Dead app navigation from /docs.** `Nav.Current` is empty on docs pages, so the layout tabs render `href="/p/"`, `/p//approvals`, `/p//resources` (all 404), the pipeline switcher is a bare "▾", and the `/` palette fetches `/p//search`. The evaluator's route from "convinced" to "trying it" is severed at the moment of maximum intent. **Fix:** populate the nav with a default pipeline slug in `internal/web/docs.go` (or hide the per-pipeline tabs on global pages). **Command:** /impeccable harden
- **[P1] Mobile horizontal blowout.** No scroll wrapper on `.docs table`: conformance.md is ~6.2× the viewport wide at 390px, agents.md 2.2×; the whole document pans, so prose is unreadable too. The app already owns a `.tablewrap` pattern — docs just doesn't get it. **Fix:** `display:block; overflow-x:auto` on `.docs table` (or wrap tables in the renderer), `overflow-wrap:anywhere` on heading code spans. **Command:** /impeccable adapt
- **[P1] Five silently broken cross-page anchors.** goldmark's auto-IDs hyphenate underscores (`max_visits` → `max-visits`) while the hand-written anchors use GitHub's algorithm, which preserves them — so links work on GitHub and silently jump to page-top in the web UI; plus one stale `#cli-backed-agents` from a heading rename. A repo that CI-tests every YAML block ships untested anchors. **Fix:** implement GitHub-style heading IDs for goldmark (small `parser.IDs` impl) AND add a link-integrity test to `docs_test.go` (same shape as `TestDocsPagesListed`); fix the stale anchor. **Command:** /impeccable harden
- **[P2] CLI rendering is unbranded and drops content.** `WithWordWrap(100)` hardcoded wraps at ~98 cols in an 80-col terminal (tables shear, identifiers split mid-word); glamour's auto style truncates table cells (README's control-flow row loses the word "approvals" entirely); headings are another product's purple. **Fix:** wrap at `min(termWidth, 100)`; a small custom glamour StyleConfig on the app palette (green headings, no background pills); page long output through `$PAGER`. **Command:** /impeccable polish
- **[P2] No in-page wayfinding on long pages.** agents.md is 14,040px tall / 848 terminal lines, 30 headings, no TOC, no back-to-top, rail only at top. Reference readers arrive mid-topic; their only tool is Ctrl-F. **Fix:** server-side TOC of h2s under the docsnav (goldmark already emits the ids); `scroll-margin-top` on headings. **Command:** /impeccable layout

## Persona Red Flags

**Terminal power user (the declared core audience):** gets the worst rendering — purple defaults, 100-col wrap shearing in an 80-col tmux pane, table cells silently dropping words, 848 unpaged lines, links rendered as inline noise. The product's manifesto says this person is home; the docs treat them as an afterthought.

**First-time evaluator (web):** strong landing (purpose, reading order, LENGTH column — knows what/where/next in one screen), then two traps: red YAML walls that pattern-match "failure" before they read "syntax," and the P0 404 when they click "jobs" to try the product.

**Mobile skimmer (colleague's link):** conformance.md is effectively unusable (6× pan); README's tables clip; only pure-prose pages survive.

## Minor Observations

- Line length: ~123–135 chars/line in the 880px sans measure — above the comfortable band; tighten the measure or bump body size.
- 11.96px table cells and 11px chrome sit under the 12px floor; gruvbox comment gray on `#282828` is ~4.1:1 at ~11.7px — marginal, and comments carry real semantics in these examples.
- Web 404 says only "no such doc page"; the CLI's lists every page. Share the courtesy both ways, and drop the CLI's "(see Pages)" Go-internals leak.
- CLI index renders every table link twice via footnotes (`[1]: resources.md resources.md` ×12) — noise under the corpus's best nav table.
- docsnav is flat and alphabetical; README already defines the grouping (Writing pipelines / Reference) — the rail could reuse it.
- No skip-to-content link; scrollable `<pre>` blocks lack `tabindex="0"` for keyboard scrolling.
- Detector's "single font / flat hierarchy" banners are largely false positives against the deliberate mono+sans compact system; the line-length and tiny-text findings are real.

## Questions to Consider

1. The CSS manifesto says this product's world *is* the terminal — why did the terminal surface get factory glamour while the browser got the design attention? If the POV is real, `steps docs` should be the flagship rendering.
2. This repo made example-correctness a tested cultural invariant but not anchor-correctness — what made one testable and not the other, when the test is 20 lines in the same file?
3. Is /docs a section of the app or a reading room? Today it half-commits: per-pipeline chrome (broken) without pipeline context. Give it the app's working nav or a reader's nav (grouped rail + TOC) — the hybrid delivers neither.
