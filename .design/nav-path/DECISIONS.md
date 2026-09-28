# Navigation as a path, docs in a footer, tabs that scroll on a phone

Three complaints, one bar: from `/`, clicking **jobs** did not say which pipeline you landed on (the switcher sat at the far right, dim); **docs** sat among a pipeline's tabs though it is not a section of one; and on a phone the bar wrapped into three rows with the switcher orphaned on its own line and the `/` palette unreachable.

## Decisions (2026-09-28)

- **The header is a path: `steps / <pipeline> ▾ / <section>`.** The switcher moves from the right edge to directly after the brand, in foreground weight, before the tabs it scopes. Every reference surveyed (Supabase, Vercel, Neon, OpenAI Platform via Mobbin) puts scope on the left and sections after it.
- **Pages above a pipeline draw no tabs.** The root, `/docs` and the error page used to borrow the first pipeline's tabs — the actual cause of "which pipeline am I on". They now show the switcher reading **pipelines ▾**, nothing selected, and no tabs. `Nav.Anchored` is the switch. Considered and rejected: keeping the borrowed tabs with the crumb in front — smaller change, same lie.
- **Docs move to a footer**, with the source link, the build version (`web.WithVersion`, fed from `cli.BuildVersion`) and `© <year> JT Archie`. Version is data, so mono; the rest is sans and faint. Sticky to the bottom of short pages; clear of a phone's home indicator.
- **On a phone the tabs are one sideways-scrolling strip**, current tab centred on load, edge fade to say there is more. Rejected: a hamburger (hides the attention badges, which are the reason to look at the bar) and a bottom tab bar (competes with the sticky action bar and costs height on every page).
- **A jump button opens the palette.** `/` was the only way in; a phone has no key. The kbd hint hides under `hover: none`.
- **44px targets under `pointer: coarse`** for every control in the bar.

## Shots

`shots/before-*` are `main` at 78d7f27; `shots/after-*` are this branch. Wide is 1280×640; phone is 390×760 at 2× with touch emulated (Chrome).
