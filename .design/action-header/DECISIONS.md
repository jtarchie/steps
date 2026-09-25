# Action header and Retry (#146)

A fetched version is said on its get's row, as JSON, not in a panel above the transcript.

This replaces the scattered layout in `.design/run-actions-and-states`. That work put an action in the paused banner, buttons in the page head, and an explanation beside every button, and it read as clutter.

## The bar

- **One action bar under the nav, on every page of a pipeline.** It sits inside `#statusline`, which every polling page already swaps live, so it stays current without extra wiring.
  - Left side: **⏸ Pause** or **▶ Unpause**.
  - Right side: what the current page's job or run can do.
- **No "trigger without cache" button.** Retry, or `steps run --force`, runs unchanged steps again.
- **Buttons explain themselves in tooltips only.** There is no text beside them.
- **A paused pipeline turns the bar blue**, as Concourse's top bar does. There is no banner. Buttons that would start a run are disabled, and their tooltip gives the reason.
- **A read-only server shows the state and no buttons.** When paused, it also shows the `steps pipeline unpause` command.

| Page | Right side |
|---|---|
| job | ↻ Trigger, Release (when the breaker holds the job) |
| run, finished | ↻ Trigger, ⟲ Retry |
| run, live | ↻ Trigger, ■ Abort |
| follow page | ■ Abort |

## Retry = `fly rerun-build`

Checked against the Concourse source (concourse/concourse @ d6e25a3). See `docs/conformance.md`.

**Where steps matches Concourse:**
- It re-runs against exactly the versions the run's builds were created with.
- `passed:` is not checked again, and no version arrived since joins the build.
- The plan is the job's current config.
- A rerun of a rerun reruns the original.
- A version that no longer exists refuses the rerun with "chosen version of input X not available".
- A rerun changes the job's status only when it reruns the job's latest build.
- A green rerun of an old build does not jump the queue downstream.

**Where steps differs on purpose (decided 2026-09-25):**
- **Retry re-runs every build of a run**, each against its own version. A steps run can hold several of Concourse's builds (a `version: every` fan-out), and Retry means "this run again". `--rerun <run>#<n>` narrows it to one build. Per-build buttons on each row were tried and dropped as confusing.
- **Retry skips the step cache.** Concourse has no cache, and without skipping it a retry of a passed build would do nothing.
- **Retry is refused while the pipeline is paused**, the same as Trigger. Concourse instead holds the rerun as pending.
- **A rerun gets a random run id plus a `rerun_of` link**, not a name like `42.1`, because steps has no build numbers.

Shots in `shots/`: before is main at 2ce348b, after is this branch.
