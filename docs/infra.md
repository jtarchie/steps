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
- **Images are pulled up front**, right after that check, rather than implicitly on first use. An implicit pull is charged to the wrong place: its progress output goes wherever the first command's stderr goes (mixed into a resource `check`'s parsed output, or into an agent's tool result), and the download counts against that step's `timeout:` — so a large image on a cold daemon can exhaust a budget meant for the work. Images already on the daemon are skipped via a local `docker image inspect`, so a warm run costs milliseconds and no network, and an image built locally that exists in no registry is found rather than pulled. An image that can't be pulled fails the run before any step starts.
- **Caching hashing**: `image` folds into the relevant node content whenever it's non-empty — unlike `inputs:`/`outputs:` (see [workspace.md](workspace.md)), an image change alters what a command actually executes against regardless of workspace mode, so the gate is on the value itself.
- **Known caveats** (documented, not solved): there's no way to override a step back to host execution once its task/agent sets an image.

### CLI agents

For a [CLI-backed agent](agents.md) (`source.model: "@claude/..."`), `image:` containerizes **the CLI process itself**, not just the tools steps serves it. That's a bigger difference than it sounds: most of a CLI agent's tools are its own natives (`Read`, `Bash`, `Edit`), which never route through steps, so without a container the working directory is their only fence. The CLI runs as a one-shot `docker run --rm` for the length of the step.

- **The tool bridge has to stay reachable.** A CLI agent's non-native tools — custom `run:` tools, `mcp_servers:` grants, and the synthesized verdict/handoff/context tools — reach the CLI over a loopback MCP server the steps process hosts. From inside a container that means `host.docker.internal`, which steps makes resolvable everywhere by adding `--add-host host.docker.internal:host-gateway` (Docker Desktop resolves it natively; this is what makes Linux Docker Engine work the same way). The bridge binds all interfaces for such a step instead of loopback only; its bearer token is what authorizes a call, unchanged. Under `network: host` none of that applies — the container shares this namespace, so the bridge stays on loopback and no `--add-host` is passed (docker rejects that combination).
- **The container is named, and removed on every exit path.** Killing the docker client does not stop the container it started, so a step that hits its `timeout:` would otherwise leave the CLI running — still spending, and still writing into the workspace the next step is about to read. Each run gets a generated name and a `docker rm -f` on the way out, including the timeout and cancellation paths.
- **`network: none` is therefore rejected at load time** for a containerized CLI agent. Cutting egress doesn't just narrow what the agent reaches, it severs the channel the step's own verdict comes back on. (On an HTTP agent, `network: none` remains a perfectly good sandbox — nothing there depends on egress.)
- **`$HOME` is a fresh per-step directory**, bind-mounted from a host temp dir and deleted when the step ends — which is also what removes the CLI's session transcript, so a containerized run leaves nothing in your own `~/.claude`. Nothing else of yours is mounted: no history, no transcripts, no settings, no `~/.claude.json`.
- **Credentials, the one platform-dependent part.** A subscription login is stored differently per OS, and only one of the two can cross into a container:
  - On **Linux**, it's `~/.claude/.credentials.json`, which steps bind-mounts **read-only** into the container's `$HOME`. Just that file. One consequence of read-only: a token refresh inside the container can't write back, so an expired token heals on your next host-side `claude` use, not here.
  - On **macOS**, it's in the login Keychain, which a container cannot read at all. There, `source.api_key_env:` is the only route.

  Setting `api_key_env:` works on both, and is the portable choice. Preflight checks that at least one route exists and says which are missing, rather than letting the CLI fail from inside the container with a logged-out error.
- **Preflight also checks the image has the CLI**, via `docker run --rm --pull=never <image> claude --version`, after the up-front image pull rather than before it (so the probe never pays for a download inside its own timeout). Pointing `image:` at something that never had the CLI installed is easy to do and otherwise invisible until the step runs. With `--no-preflight` it still fails, just later and as an ordinary exit-127 step failure.
- **A containerized CLI agent does not need the CLI on the host.** `steps validate`'s "is the binary on `PATH`" check is skipped for any agent every step of which resolves an image — the binary lives in the image, which is the point. An agent run on the host by even one step is still checked.
- **A step can end up with two containers**: the CLI's own, and the session container that serves any bridged `run:`/MCP tools. Same image, same bind-mounted workspace — so files stay consistent — but different processes. Only relevant if you grant a CLI agent custom tools.

## Container network (`network:`)

`image:` isolates a command's filesystem view but not its network — a containerized `run_shell` an agent wrote has the same egress the host does. For a step whose commands are model-generated, that is usually the isolation you actually wanted:

```yaml
agents:
- name: analyzer
  image: python:3.12
  network: none        # can read the workspace, can't reach anything
```

- Passed straight to `docker run --network`, so `none`, `host`, `bridge`, or a named network (reaching a service on a compose network is a real use) all work. The value isn't checked against a fixed set; docker reports a typo itself at container start, like any other docker-level failure.
- **Requires `image:`**, checked at load time. A host command uses the host's network, so `network: none` there would be isolation in name only — the kind of thing found in an incident review rather than at load.
- A value starting with `-` is rejected, for the same reason `user:`'s is: `--network` is passed before the `--` separator.
- Settable on `resource_types:`, `tasks:`, `agents:`, and as a step override (non-empty-wins). Invalid on `get`/`put` steps. Note that most resource types exist *to* reach the network, so this is rarely what you want on one.
- **Caching**: `network:` folds into the node's hash.

This is not a full sandbox — see [workspace.md](workspace.md) on the same point for the filesystem. A command can still reach the host filesystem by absolute path, and `network: host` opts back out entirely.

## Container privileges and limits (`privileged:`, `container_limits:`)

Both sit wherever `image:` does — a `resource_types:` entry, a `tasks:`/`agents:` entry, or a step overriding one — and both **require `image:`**. A host-executed command has no cgroup to cap and no privilege to raise, so accepting either there would promise something it does not do; that is a load error, like `network:` without an image.

```yaml
tasks:
- name: integration
  image: docker:27-dind
  privileged: true              # docker-in-docker needs it
  container_limits:
    cpu: 512                    # --cpu-shares
    memory: 2147483648          # --memory, in BYTES (2 GiB)
  run: ./run-integration.sh
```

- **`cpu:` is a share weight, not a core count.** It maps to docker's `--cpu-shares`, a *relative* weight against other containers competing for CPU — 1024 is the default, so 512 means "half the share of a default container when both are contending", and it caps nothing at all on an idle machine. The name matches Concourse rather than being renamed to something more honest, so a pipeline moving between the two means the same thing in both.
- **`memory:` is bytes**, and it is a hard cap. A container over it is OOM-killed by the kernel, which surfaces as **exit code 137** — worth knowing, since that reads as an ordinary command failure rather than as a limit being enforced.
- **Setting `container_limits:` with neither `cpu:` nor `memory:` is a load error.** It would cap nothing while reading as if it did.
- **A step's `privileged: true` wins over its task/agent, and there is no way back down.** Like `image:`, which has no spelling for "force host execution": a step needing the narrower grant does not reference that task.
- **Neither is valid on `get`/`put` steps** — a put's execution shape comes from its resource type, and a get has no task/agent to override. Set them on the `resource_types:` entry instead.

## Container user (`user:`)

On Linux, a bind mount carries host uids straight through. A container running as root — which most images do — therefore writes **root-owned files into the step's working directory**, and three things break, none of them obviously related to each other:

- An agent's file tools run host-side while its `run_shell` runs in the container, so the agent creates a file with one tool and then cannot edit it with another, mid-conversation, for no reason it can see.
- Workspace capture and cleanup hit permission errors on files the step legitimately produced.
- Whatever the step leaves behind needs root to delete, long after the run.

So **on Linux the default is the uid:gid that started `steps`**, not the image's user. Elsewhere the mismatch doesn't arise (Docker Desktop's VM maps ownership on bind mounts) and forcing a uid would instead break images whose own files belong to the user they expect — so off Linux the default stays the image's.

```yaml
tasks:
- name: install-deps
  image: ubuntu
  user: root          # this image installs packages at run time; it needs root
  run: apt-get update && apt-get install -y jq && ./script.sh
```

- **`user:` is the escape hatch, and it always wins.** Anything `docker run --user` accepts works: `root`, `1000:1000`, a username in the image.
- **The cost is real**: under the Linux default, an image that installs packages at run time or writes to a root-owned path fails. That failure is loud and local to the step — the trade being made against a silent, remote one. Reach for `user: root` when you hit it.
- Settable on `resource_types:`, `tasks:`, `agents:`, and as a step-level override (non-empty-wins, like `image:`). Invalid on `get`/`put` steps.
- A value starting with `-` is rejected at load time. Unlike `image:`, `--user` is passed *before* the `--` that makes the rest of the argv positional, so there is no separator protecting it — this check is the only thing between a tainted value and `docker run` accepting it as a flag.
- **Caching**: `user:` folds into the node's hash — running as root and running as an unprivileged user are genuinely different executions.

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
- **Downstream-trigger fan-out is version-set-blind on its own** — a dirty resource enqueues every job with a matching `trigger: true` get step. `passed:` (below) is what adds version gating on top, including requiring versions to have gone green together.
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
- **Versions must have passed TOGETHER.** When a job constrains two resources on the same upstream job, the versions it runs with must have been green in the *same* upstream build — not merely each green at some point. Two versions that passed in different builds have never been proven to work together, and a fan-in that accepts them deploys a combination nothing tested:

  ```
  upstream build 1:  repo=r2  config=c1   ok
  upstream build 2:  repo=r1  config=c2   ok

  deploy (repo + config, both passed: [upstream])
    r2 + c2  ->  held      # each was green, never together
    r1 + c2  ->  released  # green together in build 2
  ```

  This is what Concourse's scheduler does, and `steps` did not do it until the correlation was added — see [conformance.md](conformance.md). **Upgrade note:** rows recorded before this cannot vouch for a combination, so a multi-resource fan-in waits for one more upstream run after upgrading. Single-resource constraints, the common case, are unaffected.
- **A held-back job is not a lost trigger.** The version stays current, so the next poll after the upstream job goes green enqueues it. Nothing needs to be re-pushed.
- **Load-time checks.** `passed:` is get-only, may not name its own job, and may not name a job that never gets the same resource — that last one would be a deadlock spelled as a typo, since no version of a resource a job never fetches can ever pass there.

## `max_in_flight:` — how many builds of one job at once

By default a job's builds are **unlimited**, bounded only by `steps watch --max-concurrent`. Cap it per job when the work is not safe to overlap but does not need full serialization:

```yaml
jobs:
- name: integration
  max_in_flight: 2     # at most two builds of this job at a time
  plan: [...]
```

- **Unset is unlimited**, matching Concourse. The worker pool is the real backstop: `--max-concurrent` caps builds across the whole pipeline regardless.
- **`serial:`/`serial_groups:` force 1** and take precedence. Setting `max_in_flight:` alongside either is a **load error** rather than silently being overridden — the number would do nothing, and a quietly-ignored limit is worse than a rejected one. (Concourse accepts the combination and lets serial win; this is a deliberate narrowing.)
- **A job the pipeline no longer describes defaults to 1.** A queue row can outlive its job definition, and serializing something nobody can describe is the conservative reading.
- Note the same word means cell concurrency on an [`across:` step](control-flow.md#concurrent-cells-max_in_flight). That overload is Concourse's, and the two sit on different things — a job field, and a step field.

## `serial:` / `serial_groups:` — stop jobs racing each other

`steps watch --max-concurrent 4` runs jobs concurrently, and a job's own builds may overlap too (see `max_in_flight:` above). For anything that deploys, publishes, or otherwise mutates the outside world, that is a hazard:

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

## `interruptible:` — what a shutdown does to a running build

`steps watch` gets SIGTERM (a restart, a redeploy, a machine going down) while a job is mid-deploy. Whether that build is allowed to finish is the question this answers.

```yaml
jobs:
- name: deploy-prod
  plan: [...]
  # default: shutdown WAITS for a running build to finish

- name: nightly-report
  interruptible: true       # ...this one can just die
  plan: [...]
```

- **The default is to wait**, matching Concourse. Half-applying a deploy because someone restarted the watcher is the failure this exists to prevent.
- **The wait is bounded** (`nonInterruptibleGrace`, 10 minutes). An unbounded wait is a watcher that cannot be stopped, and "kill -9 the supervisor" is not a shutdown story. A job needing longer should carry its own `timeout:`, which still applies.
- **`interruptible: true` restores the older behaviour**: the build shares the watcher's context and is cancelled with it. Its queue row stays `running`, so the next startup's stale-row recovery re-queues it — nothing is lost, it is just re-run.
- **This affects `steps watch` only.** `steps run` is a person at a terminal, and ctrl-C there is always immediate; a foreground run that ignored an interrupt for ten minutes would be a worse bug than the one this prevents.

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
