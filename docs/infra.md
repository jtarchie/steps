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
- **Merkle hashing**: `image` folds into the relevant node content whenever it's non-empty — unlike `inputs:`/`outputs:` (see [workspace.md](workspace.md)), an image change alters what a command actually executes against regardless of workspace mode, so the gate is on the value itself.
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
