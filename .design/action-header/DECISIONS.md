# Action header and Retry (#146)

This replaces the scattered layout in `.design/run-actions-and-states`. That work put an action in the paused banner, buttons in the page head, and an explanation beside every button, and it read as clutter.

## The bar

- **One action bar under the nav, on every page of a pipeline.** It sits inside `#statusline`, which every polling page already swaps live, so it stays current without extra wiring.
  - Left side: **⏸ Pause** or **▶ Unpause**.
  - Right side: what the current page's job or run can do.
- **Buttons explain themselves in tooltips only.** There is no text beside them.
- **A paused pipeline turns the bar blue**, as Concourse's top bar does. There is no banner. Buttons that would start a run are disabled, and their tooltip gives the reason.
- **A read-only server shows the state and no buttons.** When paused, it also shows the `steps pipeline unpause` command.

| Page | Right side |
|---|---|
| job | ↻ Trigger, ↻ Trigger, no cache, Release (when the breaker holds the job) |
| run, finished, one build | ↻ Trigger, ⟲ Retry |
| run, fan-out (several builds) | ↻ Trigger in the bar; ⟲ Retry on each build's row |
| run, live | ↻ Trigger, ■ Abort |
| follow page | ■ Abort |

## Retry = `fly rerun-build`

Checked against the Concourse source (concourse/concourse @ d6e25a3). See `docs/conformance.md`.

**Where steps matches Concourse:**
- It re-runs one build against exactly the versions that build was created with.
- `passed:` is not checked again, and no version arrived since joins the build.
- The plan is the job's current config.
- A rerun of a rerun reruns the original.
- A version that no longer exists refuses the rerun with "chosen version of input X not available".
- A rerun changes the job's status only when it reruns the job's latest build.
- A green rerun of an old build does not jump the queue downstream.

**Where steps differs on purpose (decided 2026-09-25):**
- **Retry skips the step cache.** Concourse has no cache, and without skipping it a retry of a passed build would do nothing.
- **Retry is refused while the pipeline is paused**, the same as Trigger. Concourse instead holds the rerun as pending.
- **A rerun gets a random run id plus a `rerun_of` link**, not a name like `42.1`, because steps has no build numbers.

Shots in `shots/`: before is main at 2ce348b, after is this branch.
