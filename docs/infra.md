# Infrastructure Features

Two independent opt-in features for running pipelines beyond the simple one-shot host-execution case: containerized execution (`image:`) and cross-job downstream triggers (`steps watch`). Container examples on this page validate but aren't executed by the docs suite (they need a docker daemon); the watch/trigger examples run as shown.

## Container execution (`image:`)

By default every pipeline-defined command (a resource type's `check`/`in`/`out`, a task's `run:`, an agent's `run_shell`/custom tools) runs on the host via `sh -c`. Setting `image:` on a `resource_types:` entry, a top-level `tasks:` entry, or an `agents:` entry runs that entity's commands in a container from that image instead — **one container per step**, started on the step's first command and removed when the step ends.

```yaml noexec=docker
resource_types:
- name: releases
  image: alpine/git             # check/in/out run in this image
  config:
    check: |
      git ls-remote --tags https://github.com/jtarchie/ci.git | tail -1 | awk '{print "[{\"ref\": \""$2"\"}]"}'
    in: echo {{ .version.ref | shellquote }} > ref

resources:
- name: tags
  type: releases
  source: {}

tasks:
- name: build
  image: golang:1.26            # this task's run: (and fix-loop re-runs) run here
  inputs: [tags]
  run: cat tags/ref && go version

agents:
- name: reviewer
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }
  image: python:3.12            # this agent's run_shell/custom tools run here
  tools: [read_file, run_shell]

jobs:
- name: verify
  plan:
  - get: tags
  - task: build
    image: golang:1.25          # every one of these is a STEP-level override,
    env: [BUILD_TAG]            # applying to this step and no other use of the
    user: root                  # tasks: entry above
    network: host
    privileged: true
    container_limits:
      cpu: 512
      memory: 2147483648
  - agent: reviewer
    inputs: [tags]
    messages:
      - "Sanity-check the fetched tag."
```

- **Step-level override**: a `task`/`agent` step's own `image:` overrides the referenced `tasks:`/`agents:` entry's image for that step only. It's inherit-only — a non-empty step `image:` always wins, and there's no way to force host execution from a step when the task/agent sets one. `image:` is invalid on `get`/`put` steps (a put's image comes from its resource type).
- **Container shape**: the step starts one `docker run -d --rm` container, then runs each command as `docker exec`. The working directory is bind-mounted at its own resolved host path, so host-side readers of the same directory — an agent's `read_file`/`list_dir`, workspace capture — see exactly what a containerized command wrote. No host environment variables are passed in; the container starts from the image's own env only.
- **State persists across a step's commands.** An agent that installs a package, exports a variable, or `cd`s in one `run_shell` call sees it in the next — the calls share one container. As a fresh container per command, the two-call pattern every model reaches for (`pip install x` then `python y`) simply did not work. State does *not* carry across steps.
- **Nothing is left running.** The container is named at start, so teardown is a `docker rm -f` of a known name — on the failure and cancellation paths too. If the steps process is killed outright, the container's own keepalive expires (24h) and `--rm` reaps it.
- **Lazy**: a step whose command is skipped, or that fails before running anything, never starts a container.
- **Exit codes pass through unchanged**, including docker-level failures (125 daemon-side, 126/127 unrunnable/missing command). A bad image surfaces to an agent as ordinary tool-result data, not a crash; the failure is reported once and remembered, so it isn't re-attempted on every command in a conversation.
- **Fix agents run under the failing task's image**, not the fix agent's own `image:` — they must reproduce the exact environment that produced the failure. A fix agent's own `image:` can never take effect, so it's rejected at load time instead of silently ignored.
- **Fail-fast validation**: if any `image:` is set anywhere in the config, `RunJob` validates docker (on `PATH`, `docker info` succeeds) before planning or executing anything.
- **Images are pulled up front**, right after that check, rather than implicitly on first use — an implicit pull's progress lands in the first command's output and its download counts against that step's `timeout:`. Images already on the daemon are skipped via a local inspect, so a warm run costs milliseconds; an image that can't be pulled fails the run before any step starts.
- **Cache hashing**: `image` folds into the relevant node content whenever it's non-empty — an image change alters what a command actually executes against.

### `TMPDIR` when the daemon runs in a VM

On macOS the docker daemon runs inside a Linux VM (Docker Desktop, colima, Rancher), and **only some host paths are shared into it** — your home directory is, macOS's own `$TMPDIR` (`/var/folders/…`) is not.

steps builds each run's workspace under `$TMPDIR` and bind-mounts it into the container. If `$TMPDIR` isn't shared, that mount does not fail: **docker silently creates an empty directory** at the mount target. The container then runs against a workspace that has none of your inputs and writes results nowhere the host can see — typically surfacing as `can't create out/result.txt: nonexistent directory`, or as a step that "succeeds" and produces no outputs.

Point `TMPDIR` at a shared path before running:

```bash
export TMPDIR="$HOME/.steps-tmp" && mkdir -p "$TMPDIR"
```

Native Linux is unaffected. The same constraint applies to a CLI agent's container `$HOME` and its mounted credentials file; a credentials file whose host path isn't shared arrives as a *directory*, and the CLI reports being logged out.

### CLI agents

For a [CLI-backed agent](agents.md#cli-backed-agents-claudesonnet) (`source.model: "@claude/..."`), `image:` containerizes **the CLI process itself**, not just the tools steps serves it. Most of a CLI agent's tools are its own natives (`Read`, `Bash`, `Edit`), which never route through steps, so without a container the working directory is their only fence. The CLI runs as a one-shot `docker run --rm` for the length of the step.

- **The tool bridge stays reachable.** A CLI agent's non-native tools — custom `run:` tools, `mcp_servers:` grants, the synthesized verdict/context tools — reach the CLI over a loopback MCP server the steps process hosts. From inside a container that means `host.docker.internal`, which steps makes resolvable everywhere via `--add-host host.docker.internal:host-gateway`. Under `network: host` the container shares this namespace, so the bridge stays on loopback.
- **The container is named, and removed on every exit path** — including `timeout:` and cancellation, where the CLI would otherwise keep running and spending.
- **`network: none` is rejected at load time** for a containerized CLI agent: cutting egress severs the channel the step's own verdict comes back on. (On an HTTP agent, `network: none` remains a perfectly good sandbox.)
- **`$HOME` is a fresh per-step directory**, bind-mounted from a host temp dir and deleted when the step ends — which also removes the CLI's session transcript. Nothing of yours is mounted: no history, no transcripts, no settings.
- **Credentials, the one platform-dependent part.** On **Linux**, `~/.claude/.credentials.json` is bind-mounted **read-only** into the container's `$HOME` (a token refresh inside can't write back; an expired token heals on your next host-side use). On **macOS**, the login Keychain can't cross into a container at all — `source.api_key_env:` is the only route there, and the portable choice everywhere. Preflight checks that at least one route exists.
- **Preflight also checks the image has the CLI** (`docker run --rm --pull=never <image> claude --version`), after the up-front pull. Pointing `image:` at something without the CLI is easy and otherwise invisible until the step runs.
- **A containerized CLI agent does not need the CLI on the host** — `steps validate`'s PATH check is skipped for any agent whose every step resolves an image.

## Remote workers (`tags:`)

Every step of a job runs on the machine `steps` runs on. `tags:` places one somewhere else — a GPU box, a different OS or arch, a machine that holds hardware or credentials the orchestrator does not:

```yaml test=infra-worker
jobs:
- name: train
  assert:
    execution: [prepare, train]
    outcome: succeeded
  plan:
  - task: prepare
    outputs: [data]
    run: echo seed > data/seed.txt
  - task: train
    tags: [gpu]
    inputs: [data]
    outputs: [model]
    run: |
      echo "trained from $(cat data/seed.txt)" > model/report.txt
      echo "worker: ${STEPS_WORKER:-none}"
    assert:
      stdout: "worker: gpu"
      files: [model/report.txt]
```

The pipeline names a **capability**; the invocation names the **machine**:

```console
steps run --worker gpu=ssh://jt@gpu-box pipeline.yml
```

Keeping machines out of the pipeline file is what lets the same pipeline run on somebody else's fleet — the split Concourse draws between a step's `tags:` and a worker's advertised ones.

- **The step's tree goes with it.** Its declared `inputs:` are sent to the worker, the command runs there, and its declared `outputs:` come back — before the step's own `assert:` reads them. Nothing else travels: a large input does not get shipped home again to prove it did not change. The transfers are zstd-compressed when both ends speak it — negotiated at the handshake, so a shim built before compression existed gets the same trees uncompressed rather than a stream it cannot decode.
- **`STEPS_WORKER` names the tag** inside a placed command, and is unset for a step running locally. It is how a script tells the difference, and how the example above proves the tag took effect.
- **One tag.** Concourse intersects a step's tags against a pool of workers advertising theirs; there is no pool here, so a second tag would name a second machine and the step would have no home.
- **An unmapped tag is an error before the run starts**, not a fall back to local execution. A step that says it needs a GPU box, quietly running on a laptop, is the same broken promise `network:` without `image:` is refused for.
- **`local:`** runs the step through a shim in a child process on this machine — for trying a tagged pipeline out without a worker, and for debugging the shim itself: `--worker gpu=local:`.
- Valid on **task steps only**. Invalid on `get`/`put` steps (their commands come from the resource type), on `agent` steps and on a task with `fix:` — an agent's tools and conversation run in the orchestrator, so only its shell commands would move, leaving half a step on each machine. Invalid with `image:`: a worker runs the step's commands directly, so name a worker that already has what the step needs.
- The remote contract is **sshd and a pushed `steps` binary**, nothing else — no agent to install, no daemon to run. `steps` uploads itself over SFTP on first use, keyed by the binary's own content hash, and reuses it after that.
- **The URL's path chooses a disk** on the worker: `ssh://jt@gpu-box/mnt/fast` puts both the pushed binary and the step's tree under `/mnt/fast`. Absolute, as written. Omit it and the worker's own temp directory is used, which on a machine whose `/tmp` shares the root filesystem is how a build tree fills it.
- **Authentication** is your SSH agent, or `?identity=/path/to/key` for a specific key (an encrypted key has to go through the agent). Host keys are checked against `~/.ssh/known_hosts`, or `?known_hosts=` — never skipped.
- **A machine with no `known_hosts` entry** — one acquired on demand, used, and destroyed — is pinned by fingerprint instead: `?hostkey=SHA256:...`, exactly as `ssh-keygen -lf` prints it. Naming both it and `?known_hosts=` is an error rather than a precedence rule. A malformed fingerprint is refused when the mapping is read, not when the worker is dialed: a typo that only failed on connection is indistinguishable from the machine having been replaced, which is the alarm the pin exists to raise.
- **`~/.ssh/config` fills in what the mapping leaves out**, so a worker can be named the way you already name that machine: `--worker gpu=ssh://gpu-box` takes `HostName`, `User`, `Port`, `IdentityFile` and `UserKnownHostsFile` from the alias's own entry, `Include`s and all — the alias matched the way `ssh` matches it, lowercased first — with `%h`/`%p`/`%r`/`%d` and a leading `~/` expanded the way `ssh` expands them and a `UserKnownHostsFile` naming several files read as several files. Explicit beats ambient — anything written in the worker URL wins, and the file only answers what the URL did not. A key the URL names and `steps` cannot read is an error; one the *file* names is a candidate, so an absent or encrypted `IdentityFile` in a `Host *` block is skipped rather than fatal, exactly as `ssh` skips it, and so is a `UserKnownHostsFile` that does not exist yet — which leaves the host unknown, never unchecked. Point somewhere else with `?ssh_config=/path/to/config`, or read no file at all with `?ssh_config=none` — the spelling `ssh -F none` uses. The user file only: `/etc/ssh/ssh_config` is not read.
- **A directive outside that subset is refused, by name, rather than skipped.** `ProxyJump`, `ProxyCommand`, `ProxyUseFdpass`, `CanonicalizeHostname` and `HostKeyAlias` are not implemented, and an alias resolved for its `HostName` and then dialed *directly* would run the step on a machine you did not authorize — silently, wherever the direct route happens to work. Reading a config file partially is not the same as not reading it. Their off switches are honored rather than refused, since `ProxyJump none` under a `Host *` that sets one is a host saying dial me directly. `UserKnownHostsFile none` — and its `/dev/null` spelling of the same intent — is refused for the same reason a worker's host key is never skipped — unless `?hostkey=` already pins the key, which answers that question outright. Every one of these says `?ssh_config=none`, which dials the mapping exactly as written.
- **`Match` is understood only as `Match host` and `Match all`, matched against the alias.** A block written on any other criterion — `exec`, `user`, `localuser`, `final`, `canonical`, `tagged` — makes the whole file unreadable here, and the worker is refused rather than dialed from a file half of which was skipped; `Match exec` would mean running a command to decide, which reading a config file does not get to do. A block this *can* read is still only approximately resolved, so it is refused whenever it names a directive in the subset, `Include`s of its own included — tolerable for a block about agent forwarding, and not for one that decides which machine gets the step.
- **A worker on a different OS or architecture** needs a binary built for it: `?binary=/path/to/steps-linux-arm64`. `steps` has no Go toolchain in the field, so it cannot cross-compile one for you; `CGO_ENABLED=0` is what makes the one you build pushable.
- **A Windows worker is refused**, at the handshake, before any tree is sent. A tree crosses the wire carrying each file mode, but Windows has nothing to store `0111` in: `os.Chmod` there sets only the read-only attribute and returns no error, and `os.Stat` reports every regular file as `0666` (or `0444`). Left to run, the tree would unpack without its executable bits, the repack on the way home would read that back off the filesystem, and what returned would be a tree whose scripts are no longer scripts — with the step cache seeing content that changed for no reason a reader can point at, a later step unable to run the file, and no error anywhere. Refusing is the same call the codec makes about a fifo, for the same reason: a cache that quietly disagrees with itself is worse than a step that refuses to ship. This is a property of the worker's *filesystem*, not its CPU — a Linux or macOS worker of any architecture is unaffected — and a shim that reports no OS at all is accepted, since it has said nothing to refuse.
- **A worker that dies mid-step is redialed on the next try.** A crashed machine or a dropped tunnel fails the running command as an *error* (never as the command's own verdict), and the step's next command — an `attempts:` retry, typically — opens a fresh connection and re-sends the step's local tree, which already holds everything earlier commands fetched back. A host that could never be reached at all stays failed for the whole step: the first answer is the true one, and every re-ask would cost another timeout.
- **The run record says where each step ran.** A finished placed step carries `tag (address)` — in the web UI's step header and in `run_events` — and a step that ran locally carries nothing, so the rows that left stand out. The address only: `?identity=` and `?hostkey=` describe how to authenticate and are not written to the record. An alias is recorded as the alias, not as whatever `~/.ssh/config` resolved it to that day — the mapping is the stable name for the machine, and the resolution is a connection detail that can differ between the machines running `steps`. Nothing else can answer the question after the fact, since `tags:` is deliberately outside the hash.
- **Caching**: `tags:` does **not** fold into the node's hash. Placement decides where a step runs, not what it produces, and a tree that crossed the wire digests identically to one that never left — so retagging a step, or repointing a tag at a new machine, does not re-run work that already succeeded.

## Container network (`network:`)

`image:` isolates a command's filesystem view but not its network — a containerized `run_shell` an agent wrote has the same egress the host does. For a step whose commands are model-generated, that is usually the isolation you actually wanted:

```yaml noexec=docker
agents:
- name: analyzer
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }
  image: python:3.12
  network: none        # can read the workspace, can't reach anything
  tools: [read_file, run_shell]

jobs:
- name: analyze
  plan:
  - task: fetch
    outputs: [data]
    run: echo 42 > data/metrics.txt
  - agent: analyzer
    inputs: [data]
    messages:
      - "Analyze data/metrics.txt offline."
```

- Passed straight to `docker run --network`, so `none`, `host`, `bridge`, or a named network all work; docker reports a typo itself at container start.
- **Requires `image:`**, checked at load time. A host command uses the host's network, so `network: none` there would be isolation in name only.
- A value starting with `-` is rejected, for the same reason `user:`'s is: `--network` is passed before the `--` separator.
- Settable on `resource_types:`, `tasks:`, `agents:`, and as a step override (non-empty-wins). Invalid on `get`/`put` steps. Most resource types exist *to* reach the network, so this is rarely what you want on one.
- **Caching**: `network:` folds into the node's hash.

This is not a full sandbox — a command can still reach the host filesystem by absolute path, and `network: host` opts back out entirely.

## Container privileges and limits (`privileged:`, `container_limits:`)

Both sit wherever `image:` does, and both **require `image:`** — a host-executed command has no cgroup to cap and no privilege to raise, so accepting either there would promise something it does not do.

```yaml noexec=docker
resource_types:
- name: images                  # publishing an image needs a daemon of its own
  image: docker:27-dind
  privileged: true
  user: root                    # dind's daemon will not start unprivileged
  network: bridge               # ...and it has to reach the registry
  container_limits:
    cpu: 1024
    memory: 4294967296
  config:
    check: |
      printf '[{"tag": "latest"}]'
    out: |
      docker build -t app . && printf '{"tag": "latest"}'

resources:
- name: app-image
  type: images
  source: {}

tasks:
- name: integration
  image: docker:27-dind
  privileged: true              # docker-in-docker needs it
  network: bridge
  container_limits:
    cpu: 512                    # --cpu-shares
    memory: 2147483648          # --memory, in BYTES (2 GiB)
  run: ./run-integration.sh

agents:
- name: builder                 # an agent whose run_shell drives that daemon
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }
  image: docker:27-dind
  privileged: true
  user: root
  env: [DOCKER_HOST]            # named, never valued — see env: below
  container_limits:
    cpu: 1024
    memory: 4294967296
  tools: [run_shell]

jobs:
- name: test
  plan:
  - task: integration
  - agent: builder
    messages:
      - "Build the image and report what failed."
  - put: app-image
```

- **`cpu:` is a share weight, not a core count.** It maps to `--cpu-shares`, a *relative* weight against other containers contending for CPU — 1024 is the default, so 512 means half a default container's share, and it caps nothing on an idle machine. The name matches Concourse so a pipeline moving between the two means the same thing in both.
- **`memory:` is bytes**, and a hard cap. A container over it is OOM-killed, surfacing as **exit code 137** — worth knowing, since that reads as an ordinary command failure rather than a limit being enforced.
- **`container_limits:` with neither field is a load error** — it would cap nothing while reading as if it did.
- **A step's `privileged: true` wins over its task/agent, and there is no way back down** — like `image:`, which has no spelling for "force host execution".
- **Neither is valid on `get`/`put` steps**; set them on the `resource_types:` entry instead.

## Container user (`user:`)

On Linux, a bind mount carries host uids straight through. A container running as root — which most images do — writes **root-owned files into the step's working directory**, and three things break: an agent creates a file with a containerized tool and can't edit it with a host-side one, workspace capture hits permission errors, and whatever's left behind needs root to delete.

So **on Linux the default is the uid:gid that started `steps`**, not the image's user. Elsewhere the mismatch doesn't arise (Docker Desktop's VM maps ownership on bind mounts), so off Linux the default stays the image's own user.

```yaml noexec=docker
tasks:
- name: install-deps
  image: ubuntu
  user: root          # this image installs packages at run time; it needs root
  run: apt-get update && apt-get install -y jq && echo ready

jobs:
- name: setup
  plan:
  - task: install-deps
```

- **`user:` is the escape hatch, and it always wins.** Anything `docker run --user` accepts works: `root`, `1000:1000`, a username in the image.
- **The cost is real**: under the Linux default, an image that installs packages at run time fails, loudly and locally to the step. Reach for `user: root` when you hit it.
- Settable on `resource_types:`, `tasks:`, `agents:`, and as a step-level override (non-empty-wins). Invalid on `get`/`put` steps.
- A value starting with `-` is rejected at load time — `--user` is passed *before* the `--` separator, so this check is the only thing between a tainted value and docker accepting it as a flag.
- **Caching**: `user:` folds into the node's hash — running as root and as an unprivileged user are genuinely different executions.

## Passing environment through (`env:`)

Commands run with a deliberately narrow environment: a host command sees a fixed allowlist (`PATH`, `HOME`, locale, proxy settings — not the operator's credentials, and not `SSH_AUTH_SOCK`, which a pipeline that needs git-over-ssh opts back in by name), and a containerized command sees only its image's own environment. That default is the trust boundary: an agent directing `run_shell` should not get read access to everything the operator happened to export.

`env:` opts specific variables back in, by **name**:

```yaml
tasks:
- name: deploy
  env: [OPENROUTER_API_KEY]     # the name; the value stays in the operator's env
  run: |
    if [ "${OPENROUTER_API_KEY+set}" = set ]; then
      echo "credential reached the command"
    else
      echo "credential was filtered out"
    fi

jobs:
- name: release
  plan:
  - task: deploy
    assert:
      stdout: credential reached the command   # delete the env: line and this fails
  assert:
    execution: [deploy]
    outcome: succeeded
```

- **Names, never values.** `env: [DEPLOY_TOKEN=hunter2]` is rejected at load time, following `api_key_env:`/`webhook_token_env:`. The reason is concrete: these fields are hashed into the merkle content map, which is written to `state.db` — a literal would be persisted in cleartext.
- **Works on both execution paths.** On the host the named variables are added to the allowlist; in a container they're passed as `docker run -e NAME` (no value), so the secret never appears in an argv the host's process list would expose.
- **An unset variable contributes nothing** rather than an empty value, so a command can still tell "not configured" from "configured empty" — with the colon-less shell forms (`${VAR+set}`, as above, or `${VAR-fallback}`); `${VAR:-fallback}` collapses the two.
- **Step-level override**: a `task`/`agent` step's `env:` replaces the referenced entry's for that step only. Unlike `image:` this is *declared*-wins, not non-empty-wins — an explicit `env: []` means "nothing beyond the baseline", which is a real thing to want. Invalid on `get`/`put` steps (set it on the resource type).
- **Caching**: the variable **names** fold into the node's hash. The values do not — a value changing is the operator's environment moving under the pipeline, which steps has never claimed to hash.

## Downstream triggers (`trigger: true` + `steps watch`)

By default `steps` is a one-shot, single-job CLI. `steps watch pipeline.yml` adds a long-running mode that polls every resource named by any `get ..., trigger: true` step, across every job in the pipeline, and automatically runs whichever jobs are affected when that resource's latest version changes — including a version produced by another job's own `put`:

```yaml
resource_types:
- name: countfile
  config:
    check: |
      printf '[{"n": "%s"}]' "$(cat {{ .source.path | shellquote }} 2>/dev/null || echo 0)"
    in: cat {{ .source.path | shellquote }} > n.txt 2>/dev/null || echo 0 > n.txt
    out: |
      next=$(( $(cat {{ .source.path | shellquote }} 2>/dev/null || echo 0) + 1 ))
      echo "$next" > {{ .source.path | shellquote }}
      printf '{"n": "%s"}' "$next"

resources:
- name: counter
  type: countfile
  source: { path: counter.txt }

jobs:
- name: publish
  plan:
  - put: counter
  assert:
    execution: [counter]
    outcome: succeeded
- name: notify
  plan:
  - get: counter
    trigger: true      # steps watch runs this job when publish lands a new version
  - task: announce
    inputs: [counter]
    run: echo "counter is now $(cat counter/n.txt)"
    assert:
      stdout: counter is now     # which number depends on when the poller ran;
  assert:                        # under `steps test` the plan resolved before the put
    execution: [counter, announce]
    outcome: succeeded

assert:
  execution: [publish, notify]
```

```bash
steps watch pipeline.yml --interval 30s --max-concurrent 1
```

- **Two independent loops, connected only through a durable store-backed queue**: a **poller** checks every trigger resource on `--interval`, diffs the latest version against what's recorded, and enqueues every affected job; a **worker pool** (`--max-concurrent`, default 1) drains that queue by calling the same job runner `steps run` uses. The durable queue means a crash mid-run doesn't lose pending work.
- **At-least-once, never at-most-once**: a resource's recorded version only advances *after* every affected job is durably enqueued. If a check errors or the process crashes mid-poll, the resource stays "dirty" and is retried next poll rather than silently dropped.
- **Cold start builds the newest, seeds the rest.** A resource checked for the first time records everything it reports, marks everything below the newest as already taken, and triggers once on the newest — so a fresh (or freshly lost) state database can't mass-re-run every job the moment `watch` starts, and can't sit silent forever either. Concourse builds the single version its first check reports; this is the same outcome for a check that reports a window.
- **Dedup, ordering, and per-job concurrency**: a resource going dirty twice before a worker claims the row enqueues its affected job once — but a job already running can still get a fresh pending row queued behind it, so a version change mid-run isn't dropped. Claiming respects the job's [`max_in_flight:`](#max_in_flight--how-many-builds-of-one-job-at-once) — unlimited when unset, forced to 1 by `serial:`/`serial_groups:`.
- **Graceful-shutdown carve-out**: a job interrupted by SIGINT/SIGTERM mid-run is *not* marked failed — its row is left "running" and reset to "pending" on the next startup, recovering a hard crash and an interrupted shutdown the same way.

## Get renaming (`resource:`)

A `get` step's `resource:` names the resource to fetch when it should differ from the step's own name — mirroring Concourse's `get.resource`:

```yaml
resource_types:
- name: greetings
  config:
    check: |
      printf '[{"word": "hello"}]'
    in: echo {{ .version.word | shellquote }} > word.txt

resources:
- name: repo
  type: greetings
  source: {}

jobs:
- name: aliased
  plan:
  - get: source          # the artifact (and directory, step name, to: target) is "source"
    resource: repo       # the resource whose check/in runs is "repo"
  - task: show
    inputs: [source]
    run: cat source/word.txt
    assert:
      stdout: hello
  assert:
    execution: [repo, show]   # recorded under the RESOURCE name, not the alias
    outcome: succeeded
```

The **artifact name is the `get:` value**; the **resource fetched is `resource:`**, defaulting to the `get:` value when omitted. This lets one resource appear under a task-friendly name, or twice in a plan under two names. Pair it with a task's `input_mapping:` (see [workspace.md](workspace.md)) to feed a reusable task's pinned input name from an aliased get.

- **Triggers resolve by the underlying resource**: `steps watch` polls the *resolved* resource once no matter how many aliases reference it.
- **Load-time**: `resource:` is valid only on `get` steps and must name an existing resource.
- **Caching**: an unaliased `get` hashes byte-identically to before this feature; an aliased `get` folds the artifact name into its hash.

## Circuit breaker: `max_consecutive_failures:`

`steps watch` runs unattended, and a job that fails on every new version will keep firing on every new version — burning model spend on a failure no automatic retry is going to fix:

```yaml
jobs:
- name: nightly-summary
  max_consecutive_failures: 3
  plan:
  - task: summarize
    run: echo summarizing
    assert:
      stdout: summarizing
  assert:
    execution: [summarize]
    outcome: succeeded        # a green run also clears the breaker's count
```

```
Fri 02:00  nightly-summary failed (1/3 consecutive)
Sat 02:00  nightly-summary failed (2/3 consecutive)
Sun 02:00  nightly-summary PAUSED after 3 consecutive failures — resume with: steps jobs resume nightly-summary
```

- **It counts triggered RUNS, not the `attempts:` retries inside one** — conflating them would trip the breaker on ordinary flakiness a retry would have absorbed.
- **Consecutive, not cumulative.** A job that fails, passes, then fails is flaky, not broken. Any success resets the count.
- **Tripping is loud**: a printed line plus a `trigger.job_paused` log record.
- **An interrupted run does not count.** Ctrl-C is an operator, not a broken job.
- **Resume is manual, deliberately** (`steps jobs <pipeline> --resume <job>`). Any successful run clears the breaker — including a manual `steps run`, the natural way to confirm a fix. Unattended auto-resume would defeat the safety purpose.
- **Off by default.** A job that declares no ceiling never pauses; the count is still kept, so turning a breaker on later starts from a real number.

## How much history to keep: `run_history:`

`steps watch` runs for weeks, and every build it does writes a run row, an event per step, what each agent step spent, the full text of every agent conversation, and a cached node per step. None of that used to be cleaned up. Measured on a pipeline answering Slack mentions overnight, one build cost about **23KB** — so a hundred builds a day added a couple of megabytes a day, forever, and three quarters of it was cached nodes and agent transcripts.

`defaults.run_history:` caps it per job, keeping the newest:

```yaml
defaults:
  # Runs are much bigger than versions, so the two caps are not the same number:
  # a version is a few dozen bytes and a run is tens of kilobytes.
  run_history: 20
  version_history: 50

jobs:
- name: summarize
  plan:
  - task: write
    run: echo summarized
    assert:
      stdout: summarized
  assert:
    execution: [write]
    outcome: succeeded
```

- **Per job, not pipeline-wide.** A global cap makes the least active job the least inspectable, because a busy neighbour evicts its history. Twenty runs of `summarize` and twenty of `deploy`, not twenty between them.
- **Default 100**, or `--run-history` on `steps run`/`steps watch`. The pipeline wins when both are set, because it is the thing that knows how much its jobs write.
- **`0` keeps everything**, the same convention [every other limit here uses](attempts-timeout.md#zero-means-no-limit) — including `version_history:`. That is a real choice with a real cost, not a safe default.
- **A reaped run takes its whole story with it** — events, per-step records, agent spend and transcripts — so the run pages and `steps runs --cost` reach back exactly as far as the cap.
- **It also bounds the step cache**, but by COUNT rather than by age — cached steps are capped at a generous multiple of this number and the oldest go first. The distinction matters: a pipeline that is fully cached does no new work, so it adds no new cache entries, so nothing is ever evicted from it. Eviction happens only while new entries are being made, which is when the old ones are going stale anyway. What losing one costs is a re-run of a step whose content had not changed; nothing is recomputed *wrongly*.
- **What is NOT trimmed is what would change behavior.** Recorded resource versions have their own separate limit (`version_history:`), the per-job version cursor is never touched, and the chain-level "this content already succeeded" index is aged out on the same horizon rather than tied to node retention — so bounding history never re-answers a version a job has already handled, and never re-triggers.
- **An agent transcript is capped on its own too**, at 256KB, regardless of this setting: a conversation has as many turns as it needs, and one very long step should not be able to outweigh every other row in the database.

## `passed:` — only run against versions that are green upstream

Without it, `steps watch` will trigger `deploy` on a commit the `test` job **already failed on**, and there is no way to say otherwise. This is a correctness gap, not a convenience:

```yaml
resource_types:
- name: commits
  config:
    check: |
      printf '[{"ref": "abc123"}]'
    in: echo {{ .version.ref | shellquote }} > ref

resources:
- name: repo
  type: commits
  source: {}

jobs:
- name: unit
  plan:
  - get: repo
    trigger: true
  - task: test
    inputs: [repo]
    run: echo tests pass for "$(cat repo/ref)"
    assert:
      stdout: tests pass for abc123
  assert:
    execution: [repo, test]
    outcome: succeeded

- name: deploy
  plan:
  - get: repo
    trigger: true
    passed: [unit]           # only a version unit went green on
  - task: release
    inputs: [repo]
    run: echo deploying "$(cat repo/ref)"
    assert:
      stdout: deploying abc123
  assert:
    execution: [repo, release]   # unit ran green first, so the version was released
    outcome: succeeded

assert:
  execution: [unit, deploy]      # declaration order, which is why unit's green counts
```

```
commit abc123 → unit    FAILED
commit abc123 → deploy  waiting: no version has passed [unit] yet
commit def456 → unit    ok
commit def456 → deploy  ok
```

- **A job records the versions it fetched only when the whole job succeeds.** `passed:` means "that job ran green against this exact version"; a job that failed after its `get` proves nothing about what it fetched.
- **Per version, not per job.** A job green on `v1` does not release `v2` — "the tests passed at some point" is exactly the claim that lets a bad commit deploy.
- **`passed: [a, b]` means both**, not either.
- **Versions must have passed TOGETHER.** When a job constrains two resources on the same upstream job, the versions it runs with must have been green in the *same* upstream build — two versions that passed in different builds have never been proven to work together:

  ```
  upstream build 1:  repo=r2  config=c1   ok
  upstream build 2:  repo=r1  config=c2   ok

  deploy (repo + config, both passed: [upstream])
    r2 + c2  ->  held      # each was green, never together
    r1 + c2  ->  released  # green together in build 2
  ```

- **A held-back job is not a lost trigger.** The version stays current, so the next poll after the upstream job goes green enqueues it.
- **Load-time checks.** `passed:` is get-only, may not name its own job, and may not name a job that never gets the same resource — that last one would be a deadlock spelled as a typo.

## `max_in_flight:` — how many builds of one job at once

By default a job's builds are **unlimited**, bounded only by `steps watch --max-concurrent`. Cap it per job when the work is not safe to overlap but does not need full serialization:

```yaml
jobs:
- name: integration
  max_in_flight: 2     # at most two builds of this job at a time
  plan:
  - task: test
    run: echo testing
    assert:
      stdout: testing
  assert:
    execution: [test]
    outcome: succeeded
```

- **Unset is unlimited**, matching Concourse. The worker pool is the real backstop.
- **`serial:`/`serial_groups:` force 1** and take precedence. Setting `max_in_flight:` alongside either is a **load error** rather than silently being overridden.
- **A job the pipeline no longer describes defaults to 1** — a queue row can outlive its job definition, and serializing something nobody can describe is the conservative reading.
- Note the same word means cell concurrency on an [`across:` step](control-flow.md#concurrent-cells-max_in_flight). That overload is Concourse's; the two sit on different things.

## `serial:` / `serial_groups:` — stop jobs racing each other

`steps watch --max-concurrent 4` runs jobs concurrently. For anything that deploys, publishes, or otherwise mutates the outside world, that is a hazard:

```yaml
jobs:
- name: deploy-staging
  serial: true                  # never two builds of me at once
  serial_groups: [deploy-lock]  # and never at the same time as anyone else in the group
  plan:
  - task: deploy
    run: echo deploying staging
    assert:
      stdout: deploying staging
  assert:
    execution: [deploy]
    outcome: succeeded
- name: deploy-prod
  serial_groups: [deploy-lock]
  plan:
  - task: deploy
    run: echo deploying prod
    assert:
      stdout: deploying prod
  assert:
    execution: [deploy]
    outcome: succeeded

assert:
  execution: [deploy-staging, deploy-prod]
```

```
10:00:01  deploy-prod (v1) started
10:00:04  deploy-staging waiting: lock held by deploy-prod
10:03:20  deploy-prod (v1) done
10:03:20  deploy-staging started
```

- **`serial: true` forces one build at a time.** It is [`max_in_flight: 1`](#max_in_flight--how-many-builds-of-one-job-at-once) in Concourse's older spelling, and it does something: an unset job is unlimited. `serial: false` is just that default spelled out — writing `serial: true` beside `max_in_flight:` is a load error, since the number would do nothing.
- **The lock is taken inside the claim**, in one atomic statement — a read-then-claim would have a race exactly where the lock is supposed to be.
- **"Queued" and "blocked on a lock" look different.** A blocked job says who is holding it; otherwise a held job is indistinguishable from an idle watcher.
- **Membership is synced from the pipeline on every `steps watch` startup.** A group removed from the YAML stops holding a lock immediately.

## `interruptible:` — what a shutdown does to a running build

`steps watch` gets SIGTERM (a restart, a redeploy, a machine going down) while a job is mid-deploy. Whether that build is allowed to finish is the question this answers:

```yaml
jobs:
- name: deploy-prod
  plan:
  - task: deploy
    run: echo deploying
    assert:
      stdout: deploying
  # default: shutdown WAITS for a running build to finish
  assert:
    execution: [deploy]
    outcome: succeeded

- name: nightly-report
  interruptible: true       # ...this one can just die
  plan:
  - task: report
    run: echo reporting
    assert:
      stdout: reporting
  assert:
    execution: [report]
    outcome: succeeded

assert:
  execution: [deploy-prod, nightly-report]
```

- **The default is to wait**, matching Concourse. Half-applying a deploy because someone restarted the watcher is the failure this exists to prevent.
- **The wait is bounded** (10 minutes). A job needing longer should carry its own `timeout:`, which still applies.
- **`interruptible: true`** shares the watcher's context and is cancelled with it. Its queue row stays `running`, so the next startup re-queues it — nothing is lost, it is just re-run.
- **This affects `steps watch` only.** `steps run` is a person at a terminal, and ctrl-C there is always immediate.

## Webhook-triggered checks

`steps watch` polls on an interval: short means fast reaction and lots of API calls, long means slow reaction. A webhook removes the tradeoff — react instantly, poll rarely as a safety net:

```yaml
resource_types:
- name: commits
  config:
    check: |
      printf '[{"ref": "abc123"}]'
    in: echo {{ .version.ref | shellquote }} > ref

resources:
- name: repo
  type: commits
  source: {}
  webhook_token_env: GITHUB_WEBHOOK_TOKEN    # the variable NAME, not the token

jobs:
- name: build
  plan:
  - get: repo
    trigger: true
  - task: compile
    inputs: [repo]
    run: echo building "$(cat repo/ref)"
    assert:
      stdout: building abc123
  assert:
    execution: [repo, compile]
    outcome: succeeded
```

```bash
steps watch pipeline.yml --listen :8080
curl -X POST 'http://localhost:8080/check/repo?token=…'     # or: Authorization: Bearer …
```

- **The token is a credential, not config.** `webhook_token_env:` names an environment variable, following `api_key_env:`. A literal is rejected at load — a resource's fields are hashed into the merkle content map, so a literal token would be written to `state.db` in cleartext.
- **An unset token variable accepts nothing.** Reading an empty expectation as "no auth required" would turn a deployment mistake into an open trigger endpoint.
- **A bad token and an unknown resource are indistinguishable** (both 401) — otherwise the endpoint is a free directory of a pipeline's resource names.
- **POST only.** A GET would be triggerable by a browser preview or a link scanner.
- **The poll loop keeps running.** A webhook that is never delivered must not mean a change is never noticed.
- **A webhook treats the version as changed** even when it matches what was last recorded: the sender knows something the check output may not show yet.
