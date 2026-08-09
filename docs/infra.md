# Infrastructure Features

Two independent opt-in features for running pipelines beyond the simple one-shot host-execution case: containerized execution (`image:`) and cross-job downstream triggers (`steps watch`). See `examples/infra.yml` for a runnable demonstration of both.

## Container execution (`image:`)

By default every pipeline-defined command (a resource type's `check`/`in`/`out`, a task's `run:`, an agent's `run_shell`/custom tools) runs on the host via `sh -c`. Setting `image:` on a `resource_types:` entry, a top-level `tasks:` entry, or an `agents:` entry runs that entity's commands in a container from that image instead — **one container per step**, started on the step's first command and removed when the step ends.

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
- **Container shape**: the step starts `docker run -d --rm --init --name steps-<random> -v <cwd>:<cwd> -w <cwd> <image> sh -c 'sleep …'` once, then runs each command as `docker exec [-i] <container> sh -c <command>`. The working directory is bind-mounted at its own resolved (absolute, symlink-free) host path, so host-side readers of the same directory — an agent's `read_file`/`list_dir`, workspace capture — see exactly what a containerized command wrote. No host environment variables are passed into the container; it starts from the image's own env only.
- **State persists across a step's commands.** An agent that installs a package, exports a variable, or `cd`s in one `run_shell` call sees it in the next — the calls share one container. This is what the shape is for: as a fresh container per command, the two-call pattern every model reaches for (`pip install x` then `python y`) simply did not work, and no amount of prompting reliably prevented it. State does *not* carry across steps; each step gets its own container.
- **Nothing is left running.** The container is named at start, so teardown is a `docker rm -f` of a known name rather than a hope that a signal propagated — it runs on the failure and cancellation paths too. If the steps process is killed outright and never gets to tear down, the container's own keepalive expires (24h) and `--rm` reaps it, so an abandoned container cannot live forever.
- **Lazy**: constructing a step's runner costs nothing. A step whose command is skipped, or that fails before running anything, never starts a container.
- **Exit codes pass through unchanged**, including docker-level failures (125 for a daemon-side error, 126/127 for a command the container couldn't run/find) — treated exactly like a host command's exit code by every caller. A bad image surfaces to an agent as ordinary tool-result data, not a crash; the failure is reported once and then remembered, so a bad image isn't re-attempted on every command in a conversation.
- **Fix agents run under the failing task's image**, not the fix agent's own `image:` — so its `run_shell`/custom tools and the injected task-rerun tool reproduce the exact environment that produced the failure. A fix agent's own `image:` can therefore never take effect, so it's rejected at load time instead of silently ignored.
- **Fail-fast validation**: if any `image:` is set anywhere in the config, `RunJob` validates docker (on `PATH`, `docker info` succeeds) before planning or executing anything.
- **Caching hashing**: `image` folds into the relevant node content whenever it's non-empty — unlike `inputs:`/`outputs:` (see [workspace.md](workspace.md)), an image change alters what a command actually executes against regardless of workspace mode, so the gate is on the value itself.
- **Known caveats** (documented, not solved): on Linux, a container's default (often root) user can leave root-owned files in the bind-mounted step directory, which both complicates workspace cleanup and can make an agent's host-side `edit_file` unable to write a file its own containerized `run_shell` just created; containerized commands have unrestricted network egress; and there's no way to override a step back to host execution once its task/agent sets an image.

## Passing environment through (`env:`)

Commands run with a deliberately narrow environment: a host command sees a fixed allowlist (`PATH`, `HOME`, locale, `SSH_AUTH_SOCK`, proxy settings — not the operator's credentials), and a containerized command sees only its image's own environment. That default is the trust boundary: an agent directing `run_shell` should not get read access to everything the operator happened to export.

`env:` opts specific variables back in, by **name**:

```yaml
resource_types:
- name: registry
  image: alpine
  env: [REGISTRY_TOKEN]        # check/in/out can see it

tasks:
- name: deploy
  image: alpine
  env: [DEPLOY_TOKEN, AWS_REGION]

agents:
- name: reviewer
  env: [GH_TOKEN]              # run_shell/custom tools can see it
```

- **Names, never values.** `env: [DEPLOY_TOKEN=hunter2]` is rejected at load time, following `api_key_env:`/`webhook_token_env:`. The reason is concrete: a resource type's, task's, and agent's fields are hashed into the merkle content map, which is written to `state.db` — a literal would be persisted in cleartext.
- **Works on both execution paths.** On the host the named variables are added to the built-in allowlist; in a container they're passed with `docker run -e NAME` (no value), so the docker CLI forwards its own value and the secret never appears in an argv the host's process list would expose.
- **An unset variable contributes nothing** rather than an empty value, so a command can still tell "not configured" from "configured empty".
- **Step-level override**: a `task`/`agent` step's `env:` replaces the referenced entry's for that step only. Unlike `image:` this is *declared*-wins, not non-empty-wins — an explicit `env: []` means "nothing beyond the baseline", which is a real thing to want. Invalid on `get`/`put` steps (set it on the `resource_type`).
- **Caching**: the variable **names** fold into the node's hash, since which variables a command can see changes what it executes. The values do not — a value changing is the operator's environment moving under the pipeline, which `steps` has never claimed to hash (the same reasoning that keeps a model's weights out of an agent's hash).

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

## `serial:` / `serial_groups:` — stop jobs racing each other

`steps watch --max-concurrent 4` runs jobs concurrently. For anything that deploys, publishes, or otherwise mutates the outside world, that is a hazard:

```
10:00:01  deploy-prod (v1) started
10:00:04  deploy-prod (v2) started   ← both live, racing on the same target
```

```yaml
jobs:
- name: deploy-staging
  serial: true                  # never two builds of me at once
  serial_groups: [deploy-lock]  # and never at the same time as anyone else in the group
- name: deploy-prod
  serial_groups: [deploy-lock]
```

```
10:00:01  deploy-prod (v1) started
10:00:04  deploy-staging waiting: lock held by deploy-prod
10:03:20  deploy-prod (v1) done
10:03:20  deploy-staging started
```

- **`serial: true` is a statement of intent, not a switch.** This runner *always* serializes builds of one job — a version change enqueued mid-run waits for the in-flight build rather than racing it. Writing `serial: true` records that your pipeline depends on that guarantee. There is deliberately no `serial: false`: it would promise a parallelism this runner does not offer. (A divergence from Concourse, where builds of one job run concurrently unless `serial` says otherwise.)
- **The lock is taken inside the claim**, in one atomic statement, rather than checked beforehand. A read-then-claim would have a race exactly where the lock is supposed to be.
- **"Queued" and "blocked on a lock" look different.** A blocked job says who is holding it (`waiting: lock held by deploy-prod`); otherwise a held job is indistinguishable from an idle watcher, and an operator cannot tell a stuck pipeline from a busy one.
- **Membership is synced from the pipeline on every `steps watch` startup.** A group removed from the YAML stops holding a lock immediately — a stale one would keep two jobs apart forever with nothing in the pipeline to explain why.

## Webhook-triggered checks

`steps watch` polls on an interval: short means fast reaction and lots of API calls, long means slow reaction. A webhook removes the tradeoff — react instantly, poll rarely as a safety net.

```yaml
resources:
- name: repo
  type: git
  source: { uri: ... }
  webhook_token_env: GITHUB_WEBHOOK_TOKEN    # the variable NAME, not the token
```

```bash
steps watch pipeline.yml --listen :8080
curl -X POST 'http://localhost:8080/check/repo?token=…'     # or: Authorization: Bearer …
```

This is the **lowest-urgency** thing in the roadmap it came from: polling already works, so it is a latency and rate-limit optimization rather than a correctness gap.

- **The token is a credential, not config.** `webhook_token_env:` names an environment variable, following `api_key_env:`. A literal is rejected at load, for a sharper reason than usual: a resource's fields are hashed into the merkle content map, so a literal token would be written to `state.db` in cleartext.
- **An unset token variable accepts nothing.** Reading an empty expectation as "no auth required" would turn a deployment mistake into an open trigger endpoint.
- **A bad token and an unknown resource are indistinguishable** (both 401, authenticated before anything else). Otherwise the endpoint is a free directory of a pipeline's resource names to anyone who can reach it.
- **POST only.** A GET would be triggerable by a browser preview or a link scanner.
- **The poll loop keeps running.** A webhook that is never delivered — a failure, a restart mid-flight — must not mean a change is never noticed.
- **A webhook treats the version as changed** even when it matches what was last recorded: the sender knows something the check output may not show yet.
