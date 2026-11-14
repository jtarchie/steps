# Design Review: stepsci.dev (landing, thanks, docs)

Reviewed against: no brief; the site's own system (the steps web UI carried onto the web: warm near-black, hairline panes, mono for data, ANSI status palette)
Date: 2026-11-13
Source: live https://stepsci.dev, captured with cloakbrowser 0.6.5 (0.7.x needs a license key)

## Screenshots Captured

| Screenshot | Breakpoint | Description |
| --- | --- | --- |
| `screenshots/review-{index,thanks,docs-index,docs-complete}-{mobile-375,tablet-768,desktop-1280}.png` | all three | full page |
| `screenshots/review-docs-{agents,control-flow,web}-*.png` | all three | first viewport |
| `screenshots/detail-index-*.png` | 1280 / 375 | native-resolution slices of each landing section |
| `screenshots/detail-docs-agents-lists-desktop-1280.png` | 1280 | docs as shipped: lists and paragraph spacing flattened |
| `screenshots/detail-docs-agents-without-appcss-desktop-1280.png` | 1280 | the same page with `/app.css` blocked, showing what it should look like |
| `screenshots/detail-docs-index-table-clipped-1280.png` | 1280 | docs tables cut mid-word |
| `screenshots/metrics.json` | all | measured: horizontal overflow, text < 12px, targets < 32px tall |

Dark only (no light theme): `data-theme="dark"` is hard-set and `color-scheme: dark`, so there are no dark-mode variants to capture.

## Summary

There is no horizontal overflow at any width. Most of the reported "padding is off" comes from one cause: every page loads the platform's `/app.css` (Tailwind preflight + daisyUI). It reaches into the docs, which were written assuming browser defaults, and into the landing page wherever a class name happens to match a daisyUI component (`.card`, `.btn`, `.tabs`, `.tab`). The rest are local layout bugs: the email form, a cropped thumbnail, and card bodies starting at different heights.

## Status (2026-11-13)

Fixed live: every Must Fix, plus Should Fix 1, 2, 3, 5, 6 and 7. Open: Should Fix 4 (section rhythm, judged acceptable on re-measure), Should Fix 8 and 9 (keyboard labels, home link), and all of Could Improve.

- `--faint` is #7a7f72 on the landing and thanks pages: 4.54:1 on the background, 4.27:1 on panels. Every use there was text, so "split by role" came down to a single lift. The docs already map `--faint` to `--dim`.
- The idea cards use subgrid: titles, bodies (y=1794) and "example" links (y=1970) line up across all three.
- The docs TOC is `<details open>` with an inline script that closes it under 768px. With no JS it stays open.

- The host re-injects `/app.css` into every page, so removing the link does nothing. Docs fix 1 is instead an override block at the end of `docs/steps.css`. A computed-style diff (with vs without `/app.css`) went from 217 differing properties to 0 on six docs pages at 1280 and 375.
- The email form uses Turnstile `data-appearance="interaction-only"` on its own flex line, still inside the form. The response token is still issued.
- The re-measure over all 21 docs pages found an older overflow on `docs/infra.html` at 375 (an unbreakable URL in a list), fixed with `.docs { overflow-wrap: break-word }`. Now 0 of 66 page/width combinations overflow.
- `stepsci.dev` caches `docs/steps.css` at the edge for up to 1h (`s-maxage=3600`); the origin `stepsci.apps.topbanana.dev` has the fix now.
- The same `td { white-space: nowrap }` exists in the repo's `internal/web/static/app.css`, so `steps web`'s `/docs` likely shows the clipped tables too. Not changed.

## Must Fix

1. **Docs: Tailwind preflight erases prose spacing and lists.** `docs/*.html` loads `steps.css` and then `/app.css`, and `steps.css` sets no `p`/`ul`/`ol`/`li` margins or padding (it relies on browser defaults). So paragraphs touch, numbered lists lose their numbers, and nested lists lose their indent: the execution-flow steps on agents.html read as one flat run of sentences. See `detail-docs-agents-lists-desktop-1280.png` against `detail-docs-agents-without-appcss-desktop-1280.png`. _Fix: remove `<link rel="stylesheet" href="/app.css">` from all 21 docs pages. They use no Tailwind classes, and with it gone the header `.tabs/.tab` also stops picking up daisyUI sizing (15px tabs next to a 12px brand)._
2. **Docs: tables clipped mid-word, third column hidden.** The global `td { white-space: nowrap }` (meant for the web UI's data tables) applies inside `.docs`, so on docs/index.html "What it covers" runs off under a scroller at 1280 and the "Length" column is never seen. See `detail-docs-index-table-clipped-1280.png`. _Fix: `.docs td { white-space: normal; }` in `steps.css`. The same rule ships in `internal/web/static/app.css`, so `steps web`'s `/docs` likely has this bug too._
3. **Landing: email form misaligned.** The Turnstile widget (`.cf-turnstile`, 300×71, invisible in its managed mode) sits inside the flex row between the input and the button. It stretches the input to 71px tall against a 40px button, pushes "Notify me" to the far right on desktop, and leaves a ~90px empty band on mobile. See `detail-index-notify-desktop-1280.png` and `detail-index-notify-mobile-375.png`. _Fix: move the widget out of the row (after the form row), give the row `align-items: center`, and keep the widget collapsed when it renders nothing._
4. **Contrast: `--faint` #565b50 is 2.68:1 on the background and 2.52:1 on panels** (AA needs 4.5:1). It carries real content: card metadata (`.shape`), the pane header right side, prompt suffixes, code comments, the email placeholder and the footer copyright. _Fix: lift `--faint` to roughly 4.5:1 while keeping it below `--dim` (5.1:1)._
5. **Landing: jobs-graph thumbnail crops off its own subject.** `assets/jobs-graph.png` is 1280×400 and `.shots img` forces it into 16:10 with `object-fit: cover; object-position: top`. The centered crop removes the `unit`/`lint` nodes and shows tab chrome instead. See `detail-index-shots-desktop-1280.png`. _Fix: `object-position: left top` for that image._

## Should Fix

1. **Landing: two card styles.** `.card` matches daisyUI's card component, so the six cards in "real pipelines" and "docs" get `border-radius: 8px` and flex-column, while `.idea` panes, `.pane`, `.install` and `.notify` are square. See `detail-index-cards-desktop-1280.png`. _Fix: `border-radius: 0` on `.card` (or rename the class)._
2. **Landing: idea card bodies start at different heights.** The first title is one line and the other two wrap to two, so body text begins at y=1771 in one card and y=1794 in the others. See `detail-index-ideas-desktop-1280.png`. _Fix: a fixed title block (`min-height: 2lh` on `.idea h3` at ≥760px), or a subgrid._
3. **Landing: inconsistent box padding.** `.idea` uses 16px, `.card` 14/16px, `.notify` 16/18px, `.install` 12/16px and `.panehead` 8/14px, so text inside neighboring boxes starts at x=197 and x=199. _Fix: one inset (16px) for every content box; keep 8/14 only for the header strips._
4. **Landing: section rhythm is uneven.** Most sections use `padding-top: 56px`, but `.notify` uses `margin-top: 56px` after a section that already ends flush, the example panel's `.run` note is 8px from its figure, and the paragraph after the shots uses an inline `style="margin-top:18px"`. _Fix: one section gap, one figure-to-caption gap (10px, as `figcaption` already uses)._
5. **Mobile: example tab bar wraps into a ragged three-row grid** (01 | 02, 03 | 04, 05 alone, with missing right borders). See `detail-index-tabs-mobile-375.png`. _Fix: under 600px, scroll the tab bar horizontally (`flex-wrap: nowrap; overflow-x: auto`) instead of wrapping._
6. **Mobile: header wraps into two rows,** with the brand cell's right border ending mid-row and a ragged nav row (landing and docs). See `detail-index-header-mobile-375.png`. _Fix: under 600px, give the nav a full-width row with a top border instead of a trailing right border._
7. **Docs mobile: the "On this page" box is ~470px tall on agents.html** and pushes the H1 below the first screen. See `detail-docs-agents-toc-mobile-375.png`. _Fix: wrap it in `<details>`, collapsed under 768px._
8. **Keyboard: "example 03 ↓" links in the idea cards are `<label>`s,** so they can't be focused or reached with Tab. _Fix: make them `<a href="#examples">` that also check the radio (a small onclick), or keep the label and add `tabindex="0"` with an Enter handler._
9. **Header: the brand is not a link** on the landing or thanks pages, so the thanks page's only way home is an inline link. _Fix: wrap `.brand` in `<a href="/">`._

## Could Improve

1. **Tiny type:** `.eyebrow` 10.5px, `.docsgrouptitle` 10px, docs `th` 10.5px, `.shape` and mobile tab labels 11.5px. _Suggestion: 11px floor for uppercase labels, 12px for everything else._
2. **Touch targets:** footer links and docs nav links are 19 to 25px tall on mobile. _Suggestion: `padding: 8px 0` on those links under 600px._
3. **Example 01's terminal image** (`plan.png`) is a macOS window with a purple-gray background and rounded corners, the only element on the page outside the palette. _Suggestion: re-capture it in the site's terminal style, or render it as a `pre.code` pane._
4. **Docs widths:** the docs nav/TOC is 960px, content and tables 880px, prose 72ch (~635px), so there are three right edges. _Suggestion: cap `.docsnav`/`.docstoc` at the content's 880px._
5. **The install block clips the curl line** with no visible scroll affordance. _Suggestion: a right-edge fade on `.install` when it overflows, or break the pipe onto a continuation line._

## What Works Well

- No horizontal page overflow at 375, 768 or 1280: every wide element scrolls inside its own box.
- A coherent, distinctive aesthetic: the statusline, `$ prompt` section headings and hairline panes read as the product's own UI, not a template.
- `prefers-reduced-motion` is honored for the blinking cursor; there's a visible `:focus-visible` ring; the screenshots have real alt text; the form label exists (visually hidden).
- The radio-tab examples work with no JS and arrow keys.
