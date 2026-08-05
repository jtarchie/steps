# Infrastructure Features

Two independent opt-in features for running pipelines beyond the simple one-shot host-execution case: containerized execution (`image:`) and cross-job downstream triggers (`steps watch`). See `examples/infra.yml` for a runnable demonstration of both.

## Container execution (`image:`)

By default every pipeline-defined command (a resource type's `check`/`in`/`out`, a task's `run:`, an agent's `run_shell`/custom tools) runs on the host via `sh -c`. Setting `image:` on a `resource_types:` entry, a top-level `tasks:` entry, or an `agents:` entry runs that entity's commands in a fresh `docker run --rm --init` container from that image instead — one container per command, not a long-lived one.

```yaml
resource_types:
- name: git-alpine
  image: alpine/git       # check/in/out run in this image
  config: { check: ..., in: ..., out: ... }

tasks:
- name: build
  run: go build ./...
  image: golang:1.26      # this task's run: (and fix-loop re-runs) run in this image

agents:
- name: reviewer
  image: python:3.12      # this agent's run_shell/custom tools run in this image
```

- **Step-level override**: a `task`/`agent` step's own `image:` overrides the referenced `tasks:`/`agents:` entry's image for that step only. It's inherit-only — a non-empty step `image:` always wins, and there's no way to force host execution from a step when the task/agent sets one. `image:` is invalid on `get`/`put` steps (a put's image comes from its resource type).
- **Container shape**: `docker run --rm --init [-i] -v <cwd>:<cwd> -w <cwd> <image> sh -c <command>`. The working directory is bind-mounted at its own resolved (absolute, symlink-free) host path, so host-side readers of the same directory — an agent's `read_file`/`list_dir`, workspace capture — see exactly what a containerized command wrote. No host environment variables are passed into the container; it starts from the image's own env only.
- **Exit codes pass through unchanged**, including docker-level failures (125 for a daemon-side error, 126/127 for a command the container couldn't run/find) — treated exactly like a host command's exit code by every caller. A bad image surfaces to an agent as ordinary tool-result data, not a crash.
- **Fix agents run under the failing task's image**, not the fix agent's own `image:` — so its `run_shell`/custom tools and the injected task-rerun tool reproduce the exact environment that produced the failure. A fix agent's own `image:` can therefore never take effect, so it's rejected at load time instead of silently ignored.
- **Fail-fast validation**: if any `image:` is set anywhere in the config, `RunJob` validates docker (on `PATH`, `docker info` succeeds) before planning or executing anything.
- **Caching hashing**: `image` folds into the relevant node content whenever it's non-empty — unlike `inputs:`/`outputs:` (see [workspace.md](workspace.md)), an image change alters what a command actually executes against regardless of workspace mode, so the gate is on the value itself.
- **Known caveats** (documented, not solved): on Linux, a container's default (often root) user can leave root-owned files in the bind-mounted step directory, complicating workspace cleanup; a hard-killed docker CLI client can leave an orphaned container running until the daemon reaps it; there's no way to override a step back to host execution once its task/agent sets an image.

## Downstream triggers (`trigger: true` + `steps watch`)

By default `steps` is a one-shot, single-job CLI (`steps run pipeline.yml --job x`). `steps watch pipeline.yml` adds a long-running mode that polls every resource named by any `get ..., trigger: true` step, across every job in the pipeline, and automatically runs whichever jobs are affected when that resource's latest version changes — including a version produced by another job's own `put`, not just an externally-discovered one.

```yaml
jobs:
- name: publish
  plan:
  - put: some-resource
- name: notify
  plan:
  - get: some-resource
    trigger: true   # steps watch runs this job automatically when publish's put lands a new version
  - task: announce
    run: ...
```
```bash
steps watch pipeline.yml --interval 30s --max-concurrent 1
```

- **Two independent loops, connected only through a durable store-backed queue**: a **poller** checks every trigger resource on `--interval`, diffs the latest version against what's recorded, and — on a change — enqueues every affected job. A **worker pool** (`--max-concurrent`, default 1) drains that queue by calling `RunJob` exactly as `steps run` would. The durable, SQL-backed queue (rather than an in-memory set) means a crash mid-run doesn't lose track of pending work.
- **At-least-once, never at-most-once**: a resource's recorded version only advances *after* every job that change affects has been durably enqueued. If a later check errors, or the process crashes mid-poll, the resource stays "dirty" and is retried next poll rather than silently dropped.
- **Cold start seeds a baseline, never triggers.** A resource checked for the first time just records its current version — it's never itself "dirty" on that first check. This keeps a fresh (or freshly lost) state database from mass-re-running every job the moment `watch` starts.
- **Dedup, ordering, and per-job serialization**: a resource going dirty twice before a worker claims the row enqueues its affected job once, not twice — but a job already running can still get a fresh pending row queued behind it, so a version change mid-run isn't dropped. Claiming a job never overlaps two runs of the same job, even with `--max-concurrent > 1`.
- **Graceful-shutdown carve-out**: a job interrupted by SIGINT/SIGTERM mid-run is *not* marked failed (that would drop it forever, since only a new version change re-triggers it) — its row is left "running" and gets reset to "pending" on the next `watch` startup, recovering both a hard crash and an interrupted shutdown the same way. A job that reaches a genuine terminal state (done, or a real failure with the context still live) is finalized even if a SIGINT races the completion.
- **No `passed:`-style version-set gating across jobs** — that Concourse concept doesn't exist here; any dirty resource simply enqueues every job with a matching `trigger: true` get step.
- **CLI**: `steps run <pipeline.yml> [--job x] [--force]` (unchanged) and `steps watch <pipeline.yml> [--interval 30s] [--max-concurrent 1] [--force]`. The pre-existing flat invocation (`steps pipeline.yml --job x`) keeps parsing identically.

## Get renaming (`resource:`)

A `get` step's `resource:` names the resource to fetch when it should differ from the step's own name — mirroring Concourse's `get.resource`:

```yaml
- get: source          # the fetched artifact (and directory, step name, to: target) is "source"
  resource: repo       # the resource whose check/in runs is "repo"
```

The **artifact name is the `get:` value** (`source` here); the **resource fetched is `resource:`** (`repo`), defaulting to the `get:` value when omitted. This lets one resource appear under a task-friendly name, or twice in a plan under two names (each `get` starts a fresh triggered build, so two aliases don't share a working directory). Downstream steps name the artifact by its `get:` value — pair it with a task's `input_mapping:` (see [workspace.md](workspace.md)) to feed a reusable task's pinned input name from an aliased get.

- **Triggers resolve by the underlying resource**: `steps watch` polls the *resolved* resource once no matter how many aliases reference it, and a version change enqueues every job whose `trigger: true` get resolves to it. `resource_checks` stays keyed by resource name.
- **Load-time**: `resource:` is valid only on `get` steps and must name an existing resource.
- **Caching**: an unaliased `get` hashes byte-identically to before this feature; an aliased `get` folds the artifact name into its hash so two aliases of the same resource (identical source/version) stay distinct.

## Circuit breaker: `max_consecutive_failures:`

`steps watch` runs unattended, and a job that fails on every new version will keep firing on every new version — burning model spend or CI minutes on a failure no automatic retry is going to fix. Nobody finds out until someone looks at a bill or a log.

```yaml
jobs:
- name: nightly-summary
  max_consecutive_failures: 3
```

```
Fri 02:00  nightly-summary failed (1/3 consecutive)
Sat 02:00  nightly-summary failed (2/3 consecutive)
Sun 02:00  nightly-summary PAUSED after 3 consecutive failures — resume with: steps jobs resume nightly-summary
Mon 02:00  nightly-summary is paused (resume with: steps jobs resume nightly-summary)
```

```bash
steps jobs pipeline.yml                              # what is paused, and since when
steps jobs pipeline.yml --resume nightly-summary     # put it back in the rotation
```

The decisions behind it:

- **It counts triggered RUNS, not the `attempts:` retries inside one.** Conflating them would trip the breaker on ordinary flakiness that a retry would have absorbed — the opposite of the intent.
- **Consecutive, not cumulative.** A job that fails, passes, then fails is flaky, not broken. Any success resets the count.
- **Tripping is loud.** A breaker that trips silently defeats its own purpose; the entire point is that someone should know this stopped. Today that is a printed line plus a `trigger.job_paused` log record. When approval steps land they bring an outbound notification path (a hook firing a `put` to Slack or email), and this should use it — a log line that scrolls past is the weakest form of "someone should know".
- **An interrupted run does not count.** Ctrl-C is an operator, not a broken job.
- **Resume is manual, deliberately.** Any successful run clears the breaker — including a manual `steps run`, which is the natural way to confirm a fix. Auto-resume after a time window is explicitly deferred: unattended auto-resume defeats the safety purpose if the underlying breakage (a dead API key, say) has not actually been fixed.
- **Off by default.** A job that declares no `max_consecutive_failures:` never pauses. The count is still kept, so turning a breaker on later starts from a real number rather than pretending the history did not happen.

## `passed:` — only run against versions that are green upstream

Without it, `steps watch` will trigger `deploy` on a commit the `test` job **already failed on**, and there is no way to say otherwise. This is a correctness gap, not a convenience.

```yaml
jobs:
- name: unit
  plan:
  - get: repo
    trigger: true
  - task: test
    run: go test ./...

- name: deploy
  plan:
  - get: repo
    trigger: true
    passed: [unit, lint]     # only a version green in BOTH
  - put: production
```

```
commit abc123 → unit    FAILED
commit abc123 → deploy  waiting: no version has passed [unit lint] yet
commit def456 → unit    ok
commit def456 → lint    ok
commit def456 → deploy  ok
```

How it works, and what each choice buys:

- **A job records the versions it fetched only when the whole job succeeds.** `passed:` means "that job ran green against this exact version", and a job that failed after its `get` proves nothing about what it fetched.
- **Per version, not per job.** A job green on `v1` does not release `v2`. That is the entire point — "the tests passed at some point" is exactly the claim that lets a bad commit deploy.
- **`passed: [a, b]` means both**, not either.
- **A held-back job is not a lost trigger.** The version stays current, so the next poll after the upstream job goes green enqueues it. Nothing needs to be re-pushed.
- **Load-time checks.** `passed:` is get-only, may not name its own job, and may not name a job that never gets the same resource — that last one would be a deadlock spelled as a typo, since no version of a resource a job never fetches can ever pass there.
