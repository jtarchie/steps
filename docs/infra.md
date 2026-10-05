# Infrastructure Features

Two independent opt-in features for running pipelines beyond the simple one-shot host-execution case: containerized execution (`image:`) and cross-job downstream triggers (`steps web`). Container examples on this page validate but aren't executed by the docs suite (they need a docker daemon); the watch/trigger examples run as shown.

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
- **Container shape**: the step starts one `docker run -d --rm` container, then runs each command as `docker exec`. The working directory is bind-mounted at its own resolved host path, so host-side readers of the same directory — an agent's `read_file`/`list_dir`, workspace capture — see exactly what a containerized command wrote. No host environment variables are passed in; the container starts from the image's own env plus steps' [build metadata](#build-metadata-steps_run_id-).
- **State persists across a step's commands.** An agent that installs a package, exports a variable, or `cd`s in one `run_shell` call sees it in the next — the calls share one container. As a fresh container per command, the two-call pattern every model reaches for (`pip install x` then `python y`) simply did not work. State does *not* carry across steps.
- **Nothing is left running.** The container is named at start, so teardown is a `docker rm -f` of a known name — on the failure and cancellation paths too. If the steps process is killed outright, the container's own keepalive expires (24h) and `--rm` reaps it.
- **Lazy**: a step whose command is skipped, or that fails before running anything, never starts a container.
- **Exit codes pass through unchanged**, including docker-level failures (125 daemon-side, 126/127 unrunnable/missing command). A bad image surfaces to an agent as ordinary tool-result data, not a crash; the failure is reported once and remembered, so it isn't re-attempted on every command in a conversation.
- **Fix agents run under the failing task's image**, not the fix agent's own `image:` — they must reproduce the exact environment that produced the failure. A fix agent's own `image:` can never take effect, so it's rejected at load time instead of silently ignored.
- **Fail-fast validation**: if any `image:` is set anywhere in the config, `RunJob` validates docker (on `PATH`, `docker info` succeeds) before planning or executing anything.
- **Images are pulled up front**, right after that check, rather than implicitly on first use — an implicit pull's progress lands in the first command's output and its download counts against that step's `timeout:`. Images already on the daemon are skipped via a local inspect, so a warm run costs milliseconds; an image that can't be pulled fails the run before any step starts. The one exception is an [image a `get` fetched](#an-image-a-get-fetched), which has no reference until its get runs.
- **Cache hashing**: `image` folds into the relevant node content whenever it's non-empty — an image change alters what a command actually executes against.

### An image a `get` fetched

A step's own `image:` may name an artifact an earlier `get` in the same plan fetched, instead of an image. The step then runs in exactly the image that version names, so moving a pipeline onto a new toolchain is a new version upstream rather than an edit to the pipeline.

```yaml noexec=docker
resource_types:
- name: registry-image
  config:
    check: |
      digest=$(crane digest {{ .source.repository | shellquote }})
      echo "[{\"digest\": \"$digest\"}]"
    in: echo {{ .version.digest | shellquote }} > digest   # metadata only

resources:
- name: toolchain-image
  type: registry-image                 # version = {digest}
  source: {repository: ghcr.io/me/toolchain}

jobs:
- name: implement
  plan:
  - get: toolchain-image
  - task: build
    image: toolchain-image             # runs ghcr.io/me/toolchain@<the digest fetched>
    run: go version
```

- **The reference is `source.repository` + `@` + `version.digest`**, registry-image's shape; a resource of any type that follows it works. The repository is read only from `source:`, never from the version: a check, a webhook or a `--pin` can choose which content of the author's repository runs, never which repository. The digest must be `sha256:<64 hex>` or `sha512:<128 hex>`; anything else (a tag such as `latest`) fails the step, naming the get.
- **A digest, not a tag, is the point.** steps pulls an image only when the daemon does not already have it, so a moving tag keeps whatever copy a worker already holds. A digest cannot move.
- **The get must come earlier in the plan**, checked by `steps validate` and `steps pipeline set` like an input. It need not also be in the step's `inputs:` (as in Concourse): the daemon pulls the image, so the artifact's files are never read — an `in:` that writes only metadata is enough.
- **The name is an artifact if it is a resource's name or a get's name in the job.** Artifact names cannot contain `:`, `/` or `@`, so only a bare image name such as `alpine` can collide; the error for a resource no earlier get fetches says to rename the resource if the image was meant.
- **Pulled at step start**, for a step running on this machine: after its get, before its `when:` guard (which runs in the same image), and outside its `timeout:` and `attempts:`. A warm daemon costs a local inspect. A placed step's worker pulls it as it would any image. The transcript notes which reference the step resolved to.
- **Cache**: the resolved reference is what the step hashes, so a new digest runs the step again rather than skipping or reusing it.
- **Credentials are the operator's docker credentials**, never the resource's `source:` (Concourse would use the resource's).
- **Only a step's own `image:`** (including an agent step's override of its `agents:` entry, and a step hook's). A `tasks:`, `agents:` or `resource_types:` entry naming a resource or any job's get, and an artifact image in a job-level hook, are load errors: none of them can see a build's gets. A task's `outputs:` cannot be an image — only an earlier get of the name satisfies `steps validate` — and an artifact name arriving through a `load_var:` fails the step.

### `TMPDIR` when the daemon runs in a VM

On macOS the docker daemon runs inside a Linux VM (Docker Desktop, colima, Rancher), and **only some host paths are shared into it** — your home directory is, macOS's own `$TMPDIR` (`/var/folders/…`) is not.

steps builds each run's workspace under `$TMPDIR` and bind-mounts it into the container. If `$TMPDIR` isn't shared, that mount does not fail: **docker silently creates an empty directory** at the mount target. The container then runs against a workspace that has none of your inputs and writes results nowhere the host can see — typically surfacing as `can't create out/result.txt: nonexistent directory`, or as a step that "succeeds" and produces no outputs.

Point `TMPDIR` at a shared path before running:

```bash
export TMPDIR="$HOME/.steps-tmp" && mkdir -p "$TMPDIR"
```

Native Linux is unaffected.

### Podman

steps talks to the daemon over the docker engine API, and podman serves a compatible one — so **podman works with no pipeline changes**. `image:`, `network:`, `user:`, `env:` and the rest mean what they mean under docker. Verified by running the whole docker-backed test suite, end-to-end tests included, against podman 6.1 on a macOS `podman machine`.

The one thing to do is say where the socket is. steps finds a daemon the way `docker` does (`DOCKER_HOST`, then the selected `docker context`), and podman writes neither:

```bash
export DOCKER_HOST="unix://$(podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}')"   # macOS
export DOCKER_HOST="unix://$XDG_RUNTIME_DIR/podman/podman.sock"   # Linux, after: systemctl --user start podman.socket
```

Skip it if `podman-mac-helper` or the `podman-docker` package already put a socket at `/var/run/docker.sock`. A podman connection that is an `ssh://` URI is refused for the same reason an `ssh://` `DOCKER_HOST` is: the remote side would resolve the bind mount against its own disk.

- **Short image names resolve to Docker Hub**, as under docker: `image: alpine:3` works. (The `podman` command line may prompt for a registry; its docker-compatible API does not.)
- **`podman machine` shares macOS's `$TMPDIR`**, so the [`TMPDIR` workaround above](#tmpdir-when-the-daemon-runs-in-a-vm) is not needed there.
- **Private registries: log in where steps looks.** steps reads credentials from docker's `~/.docker/config.json`; `podman login` writes them somewhere else. Use `podman login --compat-auth-file ~/.docker/config.json <registry>` — and not `--authfile`, which rewrites that file in podman's own format and drops everything else in it, the selected `docker context` and any `credsStore` included. If the file names a `credsStore` (Docker Desktop sets one), steps asks that helper and nothing else, as docker does, so a login written beside it is never read: log in through the helper (`docker login`) instead.

Not verified, and expected to need care:

- **Rootless podman on a Linux host.** The [Linux default `user:`](#container-user-user) is the uid:gid that started steps. Rootless podman maps the *container's root* to that host user, and any other uid to a range the host user does not own — so the default likely cannot write its own working directory. If a step fails with permission errors on its workspace, set `user: root`: under rootless podman that *is* the host user, and the files come out owned correctly.
- **SELinux hosts** (Fedora, RHEL) may deny the workspace bind mount; steps does not add a `:z` relabel.

### CLI agents

For a [CLI-backed agent](agents.md#cli-backed-agents-claudesonnet) (`source.model: "@claude/..."`), `image:` containerizes the step's **tools** — exactly as it does for a hosted agent — never the CLI process itself. The CLI is always a host subprocess of the `steps` process; only the `run_shell`/custom-tool/MCP execution a bridged call performs happens inside the container, through the same `toolEnv.runner` a hosted agent's tools already run through.

That single fact is why a CLI agent's containerization has none of the machinery a "containerize the whole CLI" design would need:

- **No credentials cross into any container.** The subscription login (or `source.api_key_env:`) stays in the `steps` process's own environment, forwarded to the CLI's host-side subprocess exactly as it would be for an uncontainerized step. There is no macOS-Keychain-vs-Linux-file asymmetry to reason about, because nothing about authentication changed.
- **The bridge stays loopback, always.** The MCP server the CLI's tools reach back through never has to be dialled from inside a container — the CLI itself is never in one — so there is no `host.docker.internal` reach analysis and no wider bind to reason about.
- **`network: none` is a coherent, useful sandbox**, exactly as it is for a hosted agent: cutting the container's egress narrows what the step's shell commands can reach, and never touches the bridge the verdict comes back on, since that bridge was never inside the container.
- **The CLI is needed on the host, always** — `steps validate`'s PATH check applies to every CLI agent uniformly, whether or not any of its steps names an `image:`. An operator relying on the old containerized exemption (a CI runner with docker and no `claude` on PATH) now gets a validate failure naming the orchestrator, which is the machine that actually needs the binary.

One thing containerizing a CLI agent's tools does **not** fence: `web_fetch` is an in-process HTTP implementation on both the hosted and the CLI path, not a shell tool, so `network: none` narrows `run_shell` and custom tools without touching it.

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
    image: alpine:3
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
steps run --worker gpu=docker+ssh://jt@gpu-box pipeline.yml
```

Keeping machines out of the pipeline file is what lets the same pipeline run on somebody else's fleet — the split Concourse draws between a step's `tags:` and a worker's advertised ones.

### Two kinds of worker

A worker URL's scheme decides how much of the machine steps uses, and it is the one choice here worth making deliberately:

| Scheme | The step runs | The worker needs | Keeps anything between steps |
|---|---|---|---|
| `ssh://host/path` | **bare**, in the worker's own `sh` | sshd, a POSIX shell, `tar` | no |
| `docker+ssh://host` | in a **container** on the worker's own docker daemon | sshd, docker, the ssh user in the `docker` group | yes — inputs and outputs, by content |
| `local:` | in a container on **this** machine's daemon | docker here | yes |
| `aws://…` | in a container on the instance's daemon, reached over SSM | SSM agent, sshd, docker | yes |
| `gcp://…` | in a container on the instance's daemon, reached over IAP | sshd, docker, guest attributes | yes |

Nothing is installed on a worker and no `steps` binary goes anywhere. Earlier versions pushed one and ran it as a per-session agent (`?binary=`, `?shim=`); both options are now refused when the mapping is read, with a message naming what replaced them, because a mapping that silently ignored them would run the step somewhere other than the operator thinks.

- **`ssh://` is the smallest contract a machine can offer.** The step's tree goes over as a tar stream into a fresh directory, the command runs through the remote shell, and the declared outputs come back as a tar stream; the directory is removed when the step closes. Nothing is cached, so every step of a job re-sends its inputs — the price of asking nothing of the worker but sshd and `tar`. It cannot run `image:`: a placed step naming one is refused before the run starts, naming `docker+ssh://` as the scheme that can. The URL's path chooses the disk (below).
- **`docker+ssh://` drives the worker's dockerd itself**, through its unix socket forwarded over the ssh connection (OpenSSH's `direct-streamlocal` channel) — no second port, no TCP daemon, and no docker CLI on either end. `?sock=` names the socket when it is not `/var/run/docker.sock`. **The worker's sshd must allow TCP forwarding** (`AllowTcpForwarding local` or `yes`): OpenSSH refuses a streamlocal channel when TCP forwarding is off, `AllowStreamLocalForwarding` notwithstanding, and some distributions (alpine among them) ship it off. The ssh user must be able to open the socket, which in practice means the `docker` group. A daemon that does not answer fails the step with exactly those two requirements in the error.
- **On a docker+ worker every step is a container, so every placed step needs an `image:`** — a task, a resource type, an agent. A step without one is refused before the run starts rather than after a machine has been acquired. The step's tree lives in **docker volumes**, not in a directory, which is why the URL path that picks a disk on `ssh://` means nothing here: the daemon keeps its volumes wherever its own `data-root` is.
- **`local:`** is a docker+ worker on this machine's own daemon, found the way `docker` finds it — `DOCKER_HOST`, then the selected `docker context`. It is for trying a tagged pipeline out without a worker, and it is how this page's examples run. The path names the *worker*, not a disk: `--worker a=local:/one --worker b=local:/two` are two workers that share no cache on one daemon, which is what makes "the other worker is cold" testable on a laptop.

### What travels, and what stays

- **The step's tree goes with it, one top-level directory at a time.** Its declared `inputs:` are sent to the worker, the command runs there, and its declared `outputs:` come back — before the step's own `assert:` reads them. Nothing else travels: a large input does not get shipped home again to prove it did not change.
- **A docker+ worker keeps what it has seen, by content.** Each top-level directory of the step's tree is digested here and looked up on the worker under that digest; a hit costs no bytes, so the second step of a job that shares an input sends nothing for it, and neither does the next job. A miss is poured into a new volume once, and published under its digest only after it is whole, so a half-filled tree is never found. The step itself sees a **copy-on-write overlay** over the cached tree: its writes land in a volume of its own and the cached copy is never touched, so one cache entry serves any number of concurrent steps.
- **A hit is re-hashed on the worker before it is used.** The digest is computed by a small script in a pinned `busybox` image, on the worker, against the volume — so nothing crosses the tunnel to check it, and a volume someone edited, or one a crash left half-written, is a miss that gets refilled rather than the wrong tree. The same script digests a step's outputs where they were made. That one image is the only thing steps pulls onto a worker for itself.
- **The cache is bounded at 8 GiB per worker**, by size rather than age, oldest entries out first, never one a running step is mounting. Volumes a crashed session left behind are reclaimed once nothing names them and they are an hour old — long past any live session's first mount. Eviction removes the oldest entry first rather than the least recently used one — a hit cannot restamp a volume's labels, which docker makes immutable — and an entry evicted in the instant between another step finding it and mounting it fails that step's mount by name; the retry refills it.
- **`STEPS_WORKER` names the tag** inside a placed command, and is unset for a step running locally. It is how a script tells the difference, and how the example above proves the tag took effect.
- **One tag.** Concourse intersects a step's tags against a pool of workers advertising theirs; there is no pool here, so a second tag would name a second machine and the step would have no home.
- **An unmapped tag is an error before the run starts**, not a fall back to local execution. A step that says it needs a GPU box, quietly running on a laptop, is the same broken promise `network:` without `image:` is refused for.
- Valid on **task, get and put steps**, on an **agent step that also names an `image:`** (below), on a **resource** (below), and on a **job** or a **`do:`/`in_parallel:` block**, whose steps inherit it (below). Invalid on a task with `fix:` — the repair agent reads this machine's copy of the step while its commands would run on the worker — and, for the same reason, on a get or put whose resource type is `mcp:`- or `expr:`-backed, whose in and out do their file writing inside this process.

### Containers on a worker

- **The daemon that must exist is the worker's**, so the up-front `docker info` check cannot cover it: a machine acquired for the job does not exist when planning happens. A worker with no daemon fails the first placed step, in the daemon's own words, rather than at load time.
- **The tree is in volumes, so nothing is bind-mounted from a path the daemon might not see.** The `TMPDIR` trap described above for local containers — a path a VM-hosted daemon cannot see mounts as an empty directory — cannot happen on a docker+ worker, because the daemon owns every byte the step reads.
- **`user:` is resolved in the image.** An explicit `user:` crosses verbatim, as it does for a local container — it is a name the far end resolves, which is also how Concourse treats it. Unset defers to the image's own user, never to this machine's uid: the tree is on another machine's daemon, so the ownership mismatch the local default exists to prevent does not arise. Every volume's root is opened to any user before the step starts, so a non-root `user:` can write its own outputs; files *inside* an input arrive with the modes they were cached with, so such a user reads what is world-readable and cannot overwrite an input's files — the cache is shared across users, so ownership cannot follow the step (Concourse has the same limit).
- **An agent step needs `image:` to be placed, and so a docker+ worker**, and then its file tools travel with its shell. `read_file`, `list_dir`, `search_files`, `edit_file` and `write_file` stop running in this process and run in the same container on the worker that `run_shell` already execs into — one copy of the tree, reached the same way by both. Without `image:` there is no container to put them in, and `tags:` is refused rather than moving only the shell: a model reading one machine's filesystem while writing to another's has no way to tell, and both halves report success.
  - **The file tools use the image's own userland** — `find`, `grep`, `sed`, `cat`, `head`, `wc`, `tr`, `mkdir`, `readlink` — so nothing is mounted into the container and no binary is pushed to it. An image without them fails the step **at preparation**, naming itself and what it lacks, before a token is spent.
  - **`search_files` still decides everything the model is shown.** `find` only lists candidate paths and `grep` only reports matching lines; the prune list, the glob, `head_limit`, the per-line cap, the byte budget, `files_scanned` and the binary-file skip are all applied here, exactly as they are for an agent running locally. The model's pattern is parsed and re-rendered as a POSIX ERE rather than handed over as written — measured, `\d` is a digit class to alpine's busybox grep and the letter *d* to debian's GNU grep, and a pipeline whose search results depended on its image would be the one thing placement must not change. A pattern with no POSIX form (a non-greedy `*?`, a `\p{...}` class) comes back to the model as a tool error naming the construct, rather than silently matching something else.
  - **This is how a local `image:` agent works too.** A placed agent and a local one run the same code down to the same shell script, so there is no second path to get right, and no behaviour that appears only once a tag is added.
  - **One exception, and only one**: `read_file` may read the step's spill directory from the orchestrator. An oversized `run_shell` output is captured on this side even when the command ran elsewhere, written to a file there, and the model is handed its absolute path — that file is this process's record of the conversation and was never part of the step's tree, so it exists here and nowhere else. Every other tool, and every other path, goes to the container: anything the model can write has to land where `run_shell` can see it.
  - **A worker lost mid-conversation fails the step, and nothing starts it over.** A reclaimed spot instance is not re-placed either, as it is for a task: a fresh machine would start from the step's *inputs*, silently undoing every edit the model had made outside `outputs:`. `attempts:` on an agent is the retry of one *request to the model* (see [agents.md](agents.md)), never of the conversation, so it has nothing to say here.
  - **Refused in v1, each for the same reason — a second tree**: a grant of a **sub-agent** (the child builds its own container, which on a worker is a second copy of the tree), a **stdio `mcp_servers:` entry with a relative `cwd:`** (the server runs here, indexing files the placed agent's edits never reach), and a **CLI agent** (`@claude/...`, whose process is a subprocess of this one with its own working directory).

### Reaching the machine

- **The URL's path chooses a disk on an `ssh://` worker**: `ssh://jt@gpu-box/mnt/fast` puts the step's tree under `/mnt/fast`. Absolute, as written. Omit it and the worker's `$TMPDIR` (or `/tmp`) is used, and `/tmp` is the wrong disk in two different ways. Where it shares the root filesystem, a build tree fills it. Where systemd mounts it as **tmpfs** — the default on Amazon Linux 2023, on Fedora, and on recent Debian and Ubuntu releases — it is *memory*, capped near half the machine's RAM, competing with the build for the same resource. Name a path on a real disk for anything past a first try. Nothing measures that disk for you any more, so the same advice covers a filesystem that cannot hold an executable bit — `/mnt/c` under WSL2 (DrvFs), vfat or exfat, CIFS without unix extensions, 9p or virtiofs shares under Lima — where a tree's scripts arrive without their `0111` and come back that way, with no error anywhere: name a path on a native Linux filesystem. Each step's directory is removed when the step ends, and one a killed `steps` process left behind is swept the next time this machine reaches that worker.
- **Authentication** is your SSH agent, or `?identity=/path/to/key` for a specific key (an encrypted key has to go through the agent). Host keys are checked against `~/.ssh/known_hosts`, or `?known_hosts=` — never skipped. Both apply to `ssh://` and `docker+ssh://` alike.
- **A machine with no `known_hosts` entry** — one acquired on demand, used, and destroyed — is pinned by fingerprint instead: `?hostkey=SHA256:...`, exactly as `ssh-keygen -lf` prints it. Naming both it and `?known_hosts=` is an error rather than a precedence rule. A malformed fingerprint is refused when the mapping is read, not when the worker is dialed: a typo that only failed on connection is indistinguishable from the machine having been replaced, which is the alarm the pin exists to raise.
- **`~/.ssh/config` fills in what the mapping leaves out**, so a worker can be named the way you already name that machine: `--worker gpu=ssh://gpu-box` takes `HostName`, `User`, `Port`, `IdentityFile` and `UserKnownHostsFile` from the alias's own entry, `Include`s and all — the alias matched the way `ssh` matches it, lowercased first — with `%h`/`%p`/`%r`/`%d` and a leading `~/` expanded the way `ssh` expands them and a `UserKnownHostsFile` naming several files read as several files. Explicit beats ambient — anything written in the worker URL wins, and the file only answers what the URL did not. A key the URL names and `steps` cannot read is an error; one the *file* names is a candidate, so an absent or encrypted `IdentityFile` in a `Host *` block is skipped rather than fatal, exactly as `ssh` skips it, and so is a `UserKnownHostsFile` that does not exist yet — which leaves the host unknown, never unchecked. Point somewhere else with `?ssh_config=/path/to/config`, or read no file at all with `?ssh_config=none` — the spelling `ssh -F none` uses. The user file only: `/etc/ssh/ssh_config` is not read.
- **A directive outside that subset is refused, by name, rather than skipped.** `ProxyJump`, `ProxyCommand`, `ProxyUseFdpass`, `CanonicalizeHostname` and `HostKeyAlias` are not implemented, and an alias resolved for its `HostName` and then dialed *directly* would run the step on a machine you did not authorize — silently, wherever the direct route happens to work. Reading a config file partially is not the same as not reading it. Their off switches are honored rather than refused, since `ProxyJump none` under a `Host *` that sets one is a host saying dial me directly. `UserKnownHostsFile none` — and its `/dev/null` spelling of the same intent — is refused for the same reason a worker's host key is never skipped — unless `?hostkey=` already pins the key, which answers that question outright. Every one of these says `?ssh_config=none`, which dials the mapping exactly as written.
- **`Match` is understood only as `Match host` and `Match all`, matched against the alias.** A block written on any other criterion — `exec`, `user`, `localuser`, `final`, `canonical`, `tagged` — makes the whole file unreadable here, and the worker is refused rather than dialed from a file half of which was skipped; `Match exec` would mean running a command to decide, which reading a config file does not get to do. A block this *can* read is still only approximately resolved, so it is refused whenever it names a directive in the subset, `Include`s of its own included — tolerable for a block about agent forwarding, and not for one that decides which machine gets the step.
- **A silent connection is a dead one after about 45 seconds.** Every ssh connection steps opens sends a keepalive every 15 seconds and is closed after three go unanswered — OpenSSH's `ServerAliveCountMax`. A tunnel, SSM's above all, can otherwise sit forever after the machine behind it is gone, with the step waiting on it.
- **An EC2 instance is reached with `aws://i-0abc123def456789`**, through SSM: no inbound port, no public address, and no key or host key to configure — the instance's own agent dials out to the AWS control plane, which is what makes a worker behind NAT reachable at all. It is a docker+ worker by construction. The first time this process reaches an instance, one SSM command (`SendCommand`) waits for the instance's docker, creates a `steps` account in the `docker` group, installs this process's ephemeral ssh key there — `restrict,port-forwarding`, expiring after twelve hours, so an orchestrator that never comes back leaves nothing usable behind — and reports the instance's own ssh host key. steps then opens an SSM port-forward to the instance's **sshd on loopback port 22**, rides ssh inside it with that host key pinned, and reaches the docker socket over the ssh connection exactly as `docker+ssh://` does. IAM is the only door: the key arrives over SSM, which IAM vouches for, and the host key it pins arrives the same way. A machine EC2 already calls *running* has no registered SSM agent for another one to three minutes, and its user data may still be installing docker after that, so the dial waits for both rather than failing on the first ask — which is what an acquired worker spends most of its acquisition on. Auth is IAM (`ssm:StartSession`, `ssm:SendCommand`, `ssm:GetCommandInvocation`, `ssm:DescribeInstanceInformation` — and the `ec2:` Start/Stop/CreateFleet/CreateTags/Terminate/Describe actions if the acquisition rungs below are used), and the instance profile needs `AmazonSSMManagedInstanceCore` and nothing more — the instance holds no AWS credentials of its own. The instance needs docker (user data, or baked into the AMI), sshd (every stock Linux AMI runs it) and the SSM agent. A Windows instance is refused before anything is sent. `?region=` overrides the ambient region.
- **A worker can be acquired for the job rather than named**, on two further rungs of the same `aws://` scheme: `aws://stopped/i-0abc123def456789` starts a **parked** instance, uses it, and stops it again; `aws://launch/lt-0def4567890abcde` **launches** one from a launch template and terminates it when the job ends. The launch template owns the entire EC2 vocabulary — AMI, instance type and overrides, subnet, security groups, instance profile, user data — so steps adds no EC2 configuration surface of its own. A launched machine (`aws://launch/`, `gcp://launch/`) carries `steps-worker`/`steps-host`/`steps-pid` tags or labels, stamped in the create request itself so a crash cannot leave an unlabelled one, which is how a machine a killed process never gave back is found ([AWS](aws-workers.md#7-tear-it-down), [GCP](gcp-workers.md#6-tear-it-down)); steps never reads them back, so a label never makes a machine steps' to adopt or delete.
- **Acquisition is per JOB, never per step — and a machine is shared, not owned.** Cloud acquisition costs 20–90 seconds and real money, so the first placed step in a job pays for the machine, every later step reuses it, and a job whose placed steps are all cache hits acquires nothing at all. Under `steps web`, every job, poll and webhook delivery that maps the same worker uses **one** machine, counted: the first user starts it and the last one out gives it back — however it ends, including cancelled — so one job finishing never stops a machine another is mid-step on. "The same worker" means the same **machine**, not the same text: a parked instance is its instance and location, and a launched one is its template, version, capacity and location (region, or project and zone). The root, `?hostkey=`, `?idle=` and the order of the parameters may all differ — each mapping still connects its own way — and the shared machine is kept warm for the longest `?idle=` any of them asked for, for as long as it stays in use. So two tags whose template, version, capacity and location all agree share **one** machine, and any of those differing is a separate machine — including a location left to the environment beside the same one written out. A mapping that joins a machine acquired under a different spelling says so in the run transcript. `?idle=` keeps it that much longer after its last user, on the daemon's own timer: nothing waits the window out, and a user arriving inside it finds the machine warm. A one-shot command (`steps run`, `steps test`) has no later user to keep it for, so each job gives its machines back as it ends, whatever `?idle=` says. A parked instance steps finds **already running** is used and left running: steps stops only what it started. It warns about it in the run transcript every time, naming the instance, because nothing records what steps started across a crash: after one, every machine the previous process started is found running, adopted, and left billing until somebody stops it. A machine steps itself started and finds still running (re-acquired after an eviction) is steps' own, is not warned about, and is still parked by its last user. An unknown or misplaced worker option (`?capactiy=`, `?idle=` on a machine that already exists) is refused when the mapping is read, since a typo on the knob that decides what a machine costs must not be silently ignored.
- **`?capacity=spot|spot-then-od|od`** chooses what a launched worker asks for — **default: `od`**, stated explicitly in the request, because AWS has no "template decides" semantics for a fleet's capacity type and the knob that decides what a machine costs must not default somewhere silently. Spot uses `price-capacity-optimized` allocation, and `spot-then-od` asks for both in one request so a busy pool costs money rather than a failed job. A pool with no capacity fails with EC2's own account of why.
- **`?version=` chooses which launch-template version a launched worker is built from** — `$Default` (the default when unstated), `$Latest`, or a version number like `3`. Bare `default` and `latest` spell the first two without the `$` an unquoted shell eats, and `?version=` with no value is refused rather than read as "default", since that empty value is exactly what `?version=$Latest` degrades into. Refused off the launch rung, where there is no template to have versions.

  A launch template is not one editable blob: it is a container of **numbered, immutable versions**, each holding a complete machine shape — AMI, instance type, `BlockDeviceMappings`, subnet, security groups, instance profile, user data. `aws ec2 create-launch-template-version --source-version 1` appends a new one from a delta. **This is the whole of steps' machine-shape surface, and deliberately so**: a job that needs a 200GB disk or a bigger instance type names a version that has one, rather than steps growing a `?disk=` and an `?instance_type=` and, eventually, a second-rate copy of the EC2 API in a URL. Note that `?version=` is inert on the other two rungs for the same reason it is refused there — they name a machine whose shape was decided when it was created.

- **A Compute Engine instance is reached with `gcp://worker-1?project=my-proj&zone=us-central1-a`**, through IAP TCP forwarding to the instance's own sshd, and is a docker+ worker by construction. GCP has no SSM-shaped exec channel, so the SSH contract is the transport — the relay tunnel just carries it: the client opens an outbound websocket to Google's relay, and the relay's own range (`35.235.240.0/20`) reaches port 22 on the instance's VPC interface; the docker socket is then reached over that ssh connection exactly as `docker+ssh://` reaches one. The honest comparison with `aws://`: **no public address and no internet-reachable port**, but its firewall must admit that one Google-owned range to 22 — one rule, not zero. Authentication is minted, not configured: the orchestrator process generates an ephemeral key, installs its public half for a `steps` account through instance metadata in the expiring `google-ssh` form (the guest agent stops honoring it after 12 hours, and steps prunes expired entries whenever it installs a fresh one), and verifies the host against the SSH host keys the instance itself published to **guest attributes** — which is what makes a machine created moments ago verifiable at all. The instance's template (or metadata) must set `enable-guest-attributes=TRUE`; `?hostkey=` pins the key instead for an image where it cannot. A machine just created is usually still installing docker when its sshd first answers, so the first connection waits (up to four minutes) for `docker info` to succeed; where the guest agent created the `steps` account before the `docker` group existed, it is added to the group with the passwordless sudo the agent grants, and the connection is reopened, since only a fresh login picks up a new group. `?project=` and `?zone=` locate the instance, falling back to `GOOGLE_CLOUD_PROJECT`/`CLOUDSDK_*` and the ADC credentials' own project; auth is Application Default Credentials (`gcloud auth application-default login`), and the caller needs `roles/iap.tunnelResourceAccessor` plus `roles/compute.instanceAdmin.v1`.
- **The same two acquisition rungs exist**: `gcp://stopped/worker-1` starts a parked instance and stops it after the job (`?idle=` holds it warm, exactly as on AWS — note a stopped GCE instance reports the status `TERMINATED`, Compute Engine's word for parked); `gcp://launch/template-1` creates an instance from an **instance template** and deletes it when the job ends. The template owns the entire machine vocabulary — image, machine type, disks, network, service account, provisioning model, metadata — and two `aws://` knobs deliberately do not exist here: **no `?capacity=`**, because a GCE template *decides* its own provisioning model (a spot job names a spot template — set `instanceTerminationAction: DELETE` in it, or a preempted worker's disk keeps billing), where an EC2 fleet request cannot; and **no `?version=`**, because an instance template is one immutable object rather than a container of numbered versions — a different shape is a different template.

### When a machine goes away, and what is recorded

- **A preempted GCE worker drains exactly as a spot EC2 one does** — through the same watcher (next), polling the `preempted` flag in GCE's metadata server until it reads TRUE. A poll rather than metadata's own `wait_for_change` long-poll: without the last etag, a long-poll misses a flip between two requests until its timeout, which is longer than the notice. The warning is shorter (about thirty seconds against EC2's two minutes) and there is no rebalance-recommendation analog, so every GCE notice is terminal.
- **A reclaimed worker is not a failed step.** A spot instance learns it is going away ahead of time — about two minutes on EC2, about thirty seconds on GCE — through instance metadata and nowhere else. So on an `aws://` or `gcp://` worker, steps keeps one extra ssh command running for the life of the session: a shell loop on the machine itself (it needs `curl` there) that polls the metadata service every few seconds and prints the notice when one appears — EC2's IMDSv2 `spot/instance-action`, GCE's `preempted`. On the machine rather than in a container, because IMDSv2 answers its token request with a hop limit of one, which a container on a bridge network never receives. The loop ends with its ssh session — it watches its own stdin, since sshd signals nothing to a non-pty command when the client goes — so a crashed orchestrator never leaves it polling until the machine reboots. A failure that follows a notice is reported as an eviction rather than as the command's own verdict. If the worker is one steps **acquires** (the `stopped/` and `launch/` rungs of `aws://` or `gcp://`), the step is then re-placed on a freshly acquired machine — up to twice — **without spending the step's `attempts:` budget**, because `attempts:` is your statement about your own work and the cloud taking a machine is neither caused by it nor fixable by it. A worker that merely names a machine that already exists (`ssh://`, `aws://i-*`, `gcp://worker-1`) has nowhere else to go, so it is reported rather than retried against the host that just vanished. Re-placement is refused once the step's own `attempts:` x `timeout:` wall clock has been spent — a bound on when a fresh machine may be taken, so an eviction near the end of the budget can still finish one last round rather than being cut mid-promise. A machine kept warm by `?idle=` that does not answer its first connection when a job reuses it is treated the same way: it died while nobody was using it (a crash, a partition, someone terminating it — nothing that sends the drain notice), so it is let go and the step re-placed on a fresh one, once. There is no liveness probe, so reusing a warm machine costs no extra round trip; the price is that a transient failure on this machine's side of that first connection (expired credentials, a throttled API) costs one extra acquisition — never a leaked machine, since the one let go is still given back.

  This is a deliberate divergence from Concourse, which errors the build when a worker vanishes. Two distinctions keep it honest. A command that ran and **chose** a nonzero status still failed — a machine disappearing afterwards does not unsay an answer already given — while a command the shutdown **signalled** is the machine ending it, not the command answering, and counts as infrastructure. And an EC2 *rebalance recommendation* is only advisory: it is reported and the worker is let go once the step finishes, but it never destroys a healthy machine the way a real reclamation does.
- **A worker that dies mid-step fails the command, as an error.** A crashed machine or a dropped tunnel fails the running command as an *error* — `on_error:` fires, never `on_failure:`, since the command gave no verdict. A dropped *connection* costs only that command: the next one dials again and finds the step's tree where the worker kept it (in its volumes on a docker+ worker, in its directory on `ssh://`), so an `attempts:` retry runs against the same tree rather than a fresh one. The command the drop interrupted is killed before the next one runs, so a retry never races the attempt it replaces in that tree. A machine **reclaimed** by its cloud is the exception above, re-placed on the acquisition rungs, and so is a machine kept warm by `?idle=` that no longer answers.
- **The run record says where each step ran.** A finished placed step carries `tag (address)` — in the web UI's step header and in `run_events` — and a step that ran locally carries nothing, so the rows that left stand out. The address only: `?identity=` and `?hostkey=` describe how to authenticate and are not written to the record. An alias is recorded as the alias, not as whatever `~/.ssh/config` resolved it to that day — the mapping is the stable name for the machine, and the resolution is a connection detail that can differ between the machines running `steps`. Nothing else can answer the question after the fact, since `tags:` is deliberately outside the hash.
- **And what that machine turned out to be.** `steps runs where -p <pipeline>` reports, per placed step, the tag, the platform the worker reported (`linux/arm64` — on a docker+ worker the daemon's own, so a `local:` worker on a Mac reads `linux/…`), the identity a bare `ssh://` step ran as, how many bytes had to be pushed to it (including any piped to it from another worker, since they crossed this machine's link) and how many came back from it (zero for an output nothing here read: the tree stayed on the worker), and the machine — plus the image, if the step ran in one. Add a run id for a specific run; without one, the newest. All of it comes from the worker itself — its `uname` and `id` on `ssh://`, its daemon's own report on a docker+ worker — because nothing on the orchestrator can see it, and it is the set of answers to *it passes on my laptop and fails on the fleet*. No worker reports the filesystem its tree landed on any more — a docker+ step's tree is in volumes, and `ssh://` does not measure — so that column reads `not reported` rather than a blank that looks like an ordinary disk.

  The run page draws the same rows on each placed step's own row — see [web.md](web.md#the-transcript).

  **A tagged hook is reported too.** `on_failure:`, `ensure:` and their siblings really do acquire a machine — on `aws://launch/` that means launching and billing an instance — and it is the place an operator is least likely to expect one running. A hook is deliberately outside the merkle chain, so that a hook never gets skipped for having succeeded before; that means it has no cached node, and its row is identified by the hook's own scope (`step 0 (task "build") (on_failure hook)`) instead of a content hash.

  **Facts, never a price.** There is no cost column, deliberately: what an instance-hour actually cost is not knowable from inside a run — list prices ignore Savings Plans and Reserved Instances, a spot instance's paid price is reported by no API, and real billing lands up to a day later. A confident wrong number under `COST` is worse than no column, and anyone holding their own rate card can price these rows. This is the opposite call from `steps runs cost`, where the *provider* reports the dollars and steps only records what it was told (and shows nothing when it was told nothing).

- **Caching**: `tags:` does **not** fold into the node's hash. Placement decides where a step runs, not what it produces, and a tree that crossed the wire digests identically to one that never left — so retagging a step, or repointing a tag at a new machine, does not re-run work that already succeeded.

### A whole job, or a block, on one worker

A job that belongs on one machine says so once. `tags:` on the **job** places every step in it and every job-level hook; `tags:` on a `do:` or `in_parallel:` places the steps inside that block instead:

```yaml test=infra-job-tags
jobs:
- name: train
  tags: [gpu]
  assert:
    execution: [prepare, package, report]
    outcome: succeeded
  plan:
  - task: prepare
    image: alpine:3
    run: 'echo "worker: ${STEPS_WORKER:-none}"'
    assert:
      stdout: "worker: gpu"
  - tags: [big-disk]
    do:
    - task: package
      image: alpine:3
      run: 'echo "worker: ${STEPS_WORKER:-none}"'
      assert:
        stdout: "worker: big-disk"
  ensure:
    task: report
    image: alpine:3
    run: 'echo "worker: ${STEPS_WORKER:-none}"'
    assert:
      stdout: "worker: gpu"
```

```console
steps run --worker gpu=docker+ssh://jt@gpu-box --worker big-disk=docker+ssh://jt@storage pipeline.yml
```

- **The nearest tag wins, and tags never combine.** A step's own `tags:`, then — for a `get` or `put` — its resource's, then the nearest enclosing `do:`/`in_parallel:`, then the job's. Override, never union, so every step still has exactly one home. This is Concourse's rule ([concourse#9606](https://github.com/concourse/concourse/pull/9606), v8.3.0), with the resource slotted in where Concourse has no opinion.
- **A resource's tag beats an inherited one.** A resource tag states a network fact — the source is only reachable from there — while a job or block tag is a default. Letting the default win would move a fetch onto a machine that cannot reach the source.
- **Only `do:` and `in_parallel:` declare.** `race:`, `ensemble:` and `across:` pass an inherited tag through to what they hold but cannot declare one — wrap the block in a `do:` with `tags:` instead. The block itself runs nothing, so its row in the run record carries no worker; its steps do.
- **A step's hooks go where the step went.** The job's `tags:` reach its job-level hooks, as above, and a step's own `on_failure:`/`ensure:`/… inherit what that step resolved to: a tagged step's untagged hook runs on the step's worker, a `get`'s or `put`'s on its resource's, a tagged `do:`'s on the `do:`'s. This is Concourse's rule. It includes `on_error:`/`on_abort:`, which fire most often *because* that worker died — so a hook that must run here instead cannot opt out (see below) and is restructured: move it onto an untagged `do:` around the tagged step, whose hooks take what the `do:` resolved to, or up to the job when the job carries no tag.
- **A step that cannot be placed whole is refused, inherited tag or not** — an agent without `image:`, a CLI agent, a task with `fix:`, a get or put of an `mcp:`/`expr:`/`webhook` type. The error names where the tag came from (`inherited from job "train"`); give the step what it needs, or move the tag off the job onto a `do:` around only the steps that belong on the worker. Running it here instead would be a step silently somewhere the pipeline never said. Steps that run no command — `approval:`, `load_var:` — are not placed and ignore an inherited tag. The worker's own kind is checked too, at run start once the mapping is known: a step without an image on a docker+ worker, or with one on `ssh://`, is refused before anything runs — the same move-the-tag advice applies, or give it the image.
- **There is no opt-out yet.** `tags: []` is still refused rather than meaning "run here"; restructure with a `do:` instead. Concourse left it out on purpose too.
- **A job tag moves what its steps carry.** Every untagged `put`'s rendered `source:` and `params:`, and every task's `env:` values, reach the worker without the step saying so. Tag the resource of a deploy `put` that must stay here with nothing — or better, scope the job's tag to a `do:` around the build steps only. An agent's model credentials never move: the conversation stays on this machine and only its tools are placed. `steps runs where` and each step's row in the run record say where it went.
- **The poller is not placed by a job tag.** A resource's check runs where the **resource's** `tags:` say, whichever job reads it; state reachability on the resource.
- **The whole job needs its mapping.** An unmapped job tag is refused before anything runs, even for a job whose every step is a cache hit — the same rule as a step's own tag. Caching is unaffected: tags are outside the hash, so moving a job onto a worker this way re-runs nothing that already succeeded.

### Resources on workers

A source only reachable from a worker's network — a git host inside a VPC, a registry behind a bastion — has to be checked, fetched and pushed from there. `tags:` on the **resource** places all three:

```yaml test=infra-resource-worker
resource_types:
- name: probe
  image: alpine:3
  config:
    check: printf '[{"ref":"v1","where":"%s"}]' "${STEPS_WORKER:-here}"
    in: printf '%s/%s' {{ .version.where | shellquote }} "${STEPS_WORKER:-here}" > where.txt
    out: printf '{"ref":"pushed","where":"%s"}' "${STEPS_WORKER:-here}"

resources:
- name: repo
  type: probe
  tags: [vpc]
  source: {}

jobs:
- name: mirror
  assert:
    execution: [repo, inspect, repo, compare, repo]
    outcome: succeeded
  plan:
  - get: repo
  - task: inspect
    inputs: [repo]
    run: cat repo/where.txt
    assert:
      stdout: vpc/vpc
  - get: mirror
    resource: repo
    tags: [edge]
  - task: compare
    inputs: [mirror]
    run: cat mirror/where.txt
    assert:
      stdout: vpc/edge
  - put: repo
    inputs: [repo]
```

- **The resource's tag covers its check, in and out.** Every `get` and `put` of `repo` runs on `vpc` unless the step names a tag of its own — the `mirror` get above fetches the same resource from `edge`, while the check that found its version still ran on `vpc`: a check is the resource's, wherever the fetch goes, which is why `mirror/where.txt` reads `vpc/edge`. This is a deliberate divergence from Concourse, whose resource-level `tags:` places only the check and has to be repeated on every step: a get's own version check and its `in` have to land on the same machine, and the repetition is the papercut Concourse's docs warn about.
- **The fetched tree stays on the worker until something here reads it.** On a docker+ worker a placed `in:` fills a volume, and the worker digests it where it lies and files it under that digest, telling this machine the digest rather than sending the bytes. The same holds for a placed task's declared outputs. A later step placed on the **same** worker finds the tree by digest and mounts a copy-on-write view of it: nothing crosses. A step that runs *here* — an untagged task, a `put`, a `load_var:` or `assert: files:` reading the tree, a cache that lives on this disk (`cache.resources:`, the step cache under a durable `root:`) — pulls the tree from the worker the moment it needs it, checks it against its digest, and reads it exactly as it would a local fetch. A step placed on a *different* worker is first looked up there by digest, so a consumer that already holds the tree moves nothing; otherwise this machine pulls it from the holder into a temporary directory, checks the digest, and sends it on, the way Concourse's web node streams one worker's volume into another. The bytes cross this machine's link twice and touch its disk briefly; there is no worker-to-worker path, and `--artifact-store` is not one (below). Either way, a tree crossing from one worker to another **refuses a symlink that leaves it**, as it would landing here: its target names a path on the receiving worker. A step whose outputs are read as part of the step itself — an `assert: files:`, a `fix:` — brings them home as it always did. An `ssh://` worker holds nothing, so its outputs always come home. **A worker that no longer holds a tree fails the step that reads it, by name** — the artifact and the machine — rather than running it against nothing; the tree is gone with the machine, as it is when a Concourse worker goes.
- **The poller places checks too.** `steps web` polls with the same `--worker` mappings, on any rung: a polled check shares its machine with whatever job is using it, so a poll ending cannot stop a machine a job is mid-step on. On an acquisition rung (`aws://launch/`, `aws://stopped/`, `gcp://launch/`, `gcp://stopped/`) that means each poll that finds the machine idle starts it and gives it back again — set `?idle=` longer than `--interval` to keep it warm across polls, which is usually what you want: a short `--interval` without `?idle=` means one machine acquired per poll. A job's pre-plan freshness check does **not** start one: under `steps web`, a polled resource whose worker is acquired on demand is not re-checked before the plan, and the job builds from what the poller last recorded, saying so in its transcript. A resource nothing polls, and every resource under a one-shot `steps run`, is still checked before the plan, starting the machine if it has to — nothing else would ever move its history. An unmapped resource tag is refused before anything is polled, and `steps plan` takes `--worker` for the same checks.
- **A version is text the worker wrote.** What a placed `check:` prints becomes `{{ .version.* }}` in the `in:` that runs next — on the step's worker, or on this machine when a step overrides the resource's tag with `local:` — so a version field crossing machines is exactly the untrusted value [templating.md](templating.md#shell-quoting-untrusted-values) says to pass through `shellquote`. The example above does.
- **Shell-backed types only.** An `mcp:` or `expr:` type's in and out write their files from inside this process, so a tag could move only a fraction of the stage; the load refuses it and says so.
- **On a docker+ worker the resource type needs an `image:`**, and its check, in and out run in it on the worker's daemon — the socket the mapping names (`?sock=`, default `/var/run/docker.sock`), or for `local:` the daemon `docker` itself finds. On `ssh://` the type's commands run bare in the worker's shell, and an `image:` there is refused before the run starts, as for a task.

### The life of an `aws://` worker

Four things happen on four different clocks, and knowing which is which explains most of the behaviour above. A **placed step** is one carrying `tags:`; a **rung** is which of the three `aws://` forms you wrote.

```
steps run --worker aws=aws://...  pipeline.yml
│
├─ first placed step
│   │
│   ├─► ACQUIRE ── once per JOB, not per step
│   │     aws://i-0abc...          already running; nothing is acquired
│   │     aws://stopped/i-0abc...  StartInstances, wait for "running"
│   │     aws://launch/lt-0def...  CreateFleet, wait for "running"
│   │
│   ├─► INSTALL ── once per INSTANCE per steps process (SSM SendCommand)
│   │     wait for docker (the user data may still be installing it)
│   │     user "steps" in the docker group
│   │     this process's key: restrict,port-forwarding, expires in 12h
│   │     report the instance's ssh host key, to pin
│   │
│   ├─► CONNECT ── once per STEP
│   │     SSM port-forward to the instance's sshd on 127.0.0.1:22
│   │     ssh as "steps", host key pinned, keepalive every 15s
│   │     docker socket over that ssh connection (streamlocal)
│   │     + one ssh command polling IMDSv2 for a spot notice
│   │
│   └─► RUN
│         inputs found by digest or poured into volumes
│         the step's container runs against them
│         outputs held on the worker under their digests,
│           or read back when something here needs them
│
├─ later placed steps: reuse the machine and its install; CONNECT again
│
└─ job ends (succeeded, failed, or cancelled)
    │
    └─► RELEASE ── when the LAST user of the machine is done
          aws://i-0abc...          nothing; the machine is yours
          aws://stopped/i-0abc...  StopInstances, after ?idle= if set
          aws://launch/lt-0def...  TerminateInstances, after ?idle= if set
```

What that leaves on the instance between steps: the `steps` account and its `authorized_keys` line (expired lines are pruned by the next install), the cache volumes, the `busybox` image the digest runs in, and the images steps pulled. No steps process runs on it, ever. An install that went stale — the key expired, or the root volume was replaced under the same instance id, so the host key changed — is noticed at the next connection and done again, once; the new host key arrives over SSM, which IAM vouches for.

What the instance needs: the SSM agent, sshd (every stock Linux AMI runs both), and docker, which a stock Amazon Linux 2023 AMI does not have — install it in the launch template's user data or bake it into the AMI. Images are pulled by the instance's own daemon, so it needs a route to whichever registry the steps name.

What the instance never holds: AWS credentials. The instance profile needs `AmazonSSMManagedInstanceCore` and nothing else.

**A `gcp://` worker lives the same life on different machinery.** ACQUIRE is `instances.insert` from the template (or `instances.start` for the parked rung); INSTALL is the key written into instance metadata and the host keys read back from guest attributes, plus the one-time wait for docker; CONNECT is the IAP relay websocket carrying the SSH session, with the same socket forward and a watcher polling `preempted`; RELEASE is `instances.delete` (or `stop`). A GCE instance holds no GCP credentials of its own for steps' purposes — the orchestrator's ADC signs the tunnel. One machine-shape note with money attached: name a spot provisioning model **with `instanceTerminationAction: DELETE`** in the template, or every preempted worker leaves a stopped instance whose disk keeps billing.

## Artifact store (`--artifact-store`)

The step cache's remote half: cached step outputs mirrored to a content-addressed store on S3, so bytes evicted locally — or never present on this machine — are materialized back instead of re-earned by running the step. Opt-in, and CLI-only by design: the flag names infrastructure, and pipelines stay portable.

```console
steps run --artifact-store s3://my-bucket/team-prefix pipeline.yml
```

- **It needs a durable `workspace.root:`.** The store mirrors the step cache, and the step cache exists only under a durable root; a run without one logs that the mirror is off rather than refusing.
- **It is not a data plane for placed steps.** Earlier versions moved a placed step's trees through the bucket by presigned URL; a docker+ worker keeps trees in its own volumes by content and a tree between two workers is relayed by this machine, so the flag has no effect on where a placed step's bytes go.
- **The bucket holds bytes, never truth.** Each output travels as one zstd tarball keyed by its content digest, under `<prefix>/blobs/<digest>`. Which digest means what — the mapping from "this work over these input bytes" to "these output digests" — stays in the pipeline's own state database. That split is what makes an S3 lifecycle rule expiring untouched objects always safe: the worst case is a re-upload, never a wrong skip.
- **A machine handed the state file inherits the cache.** A fresh checkout with the `.steps/<name>.db` and the same flag skips the steps the database says succeeded, fetching their outputs by digest — which is what makes a CI runner that persists only the small state file, not the workspace, warm on its second run.
- **What arrives is verified, not trusted**: every fetched tree is re-digested before it is installed, and bytes that do not match their key are refused as an ordinary miss. Every mirror failure — store unreachable, blob expired, index unknown — costs a re-run and nothing else; no mirror failure can fail a build that is otherwise working.
- **Uploads are skipped when the store already holds the digest** (one `HEAD` per output), so an unchanged output is never re-shipped, whoever produced it first.
- **Credentials and region** come from the ambient AWS configuration — the same chain every AWS tool reads — with `?region=` as an override. `?endpoint=` points at an S3-compatible server that is not AWS (minio and friends), switching to path-style addressing.
- Applies to `run`, `test`, and `web`. `volatile:` steps are never cached, so they are never mirrored either.

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

Commands run with a deliberately narrow environment: a host command sees a fixed allowlist (`PATH`, `HOME`, locale, proxy settings — not the operator's credentials, and not `SSH_AUTH_SOCK`, which a pipeline that needs git-over-ssh opts back in by name), and a containerized command sees only its image's own environment (plus steps' [build metadata](#build-metadata-steps_run_id-)). That default is the trust boundary: an agent directing `run_shell` should not get read access to everything the operator happened to export.

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

- **Names, never values.** `env: [DEPLOY_TOKEN=hunter2]` is rejected at load time, following `api_key_env:` and a webhook resource's `secret_env:`. The reason is concrete: these fields are hashed into the merkle content map, which is written to `state.db` — a literal would be persisted in cleartext.
- **Works on both execution paths.** On the host the named variables are added to the allowlist; in a container their values are sent to the daemon in the request that creates it, so the secret never appears in an argument vector the host's process list would expose.
- **An unset variable contributes nothing** rather than an empty value, so a command can still tell "not configured" from "configured empty" — with the colon-less shell forms (`${VAR+set}`, as above, or `${VAR-fallback}`); `${VAR:-fallback}` collapses the two.
- **Step-level override**: a `task`/`agent` step's `env:` replaces the referenced entry's for that step only. Unlike `image:` this is *declared*-wins, not non-empty-wins — an explicit `env: []` means "nothing beyond the baseline", which is a real thing to want. Invalid on `get`/`put` steps (set it on the resource type).
- **Caching**: the variable **names** fold into the node's hash. The values do not — a value changing is the operator's environment moving under the pipeline, which steps has never claimed to hash.

## Build metadata (`STEPS_RUN_ID` …)

Every command inside a run — tasks, `when:` guards, hooks, `in:`/`out:`, an agent's `run_shell` and custom tools, and a CLI agent's process — gets a few variables saying which build it is part of, as Concourse's `BUILD_*` variables do. A `put` can write "built by run X, pipeline revision Y" into a PR body.

| variable | holds | Concourse |
|---|---|---|
| `STEPS_RUN_ID` | the run: the id `steps runs` and the web UI show (16 characters of `A-Z2-7`) | `BUILD_ID` |
| `STEPS_JOB_NAME` | the job | `BUILD_JOB_NAME` |
| `STEPS_PIPELINE_NAME` | the pipeline's name (`steps pipeline set -p`, or `--name`/the file's base name for `steps run`) | `BUILD_PIPELINE_NAME` |
| `STEPS_PIPELINE_REVISION` | the full SHA of the pipeline revision the run was built from — the value `steps runs` abbreviates in its CONFIG column. There is no revision *number* | — |
| `STEPS_URL` | the daemon's address, from `steps web --external-url` | `ATC_EXTERNAL_URL` |

```yaml
jobs:
- name: release
  plan:
  - task: stamp
    run: |
      printf '%s' "$STEPS_RUN_ID" | grep -Eq '^[A-Z2-7]{16}$' || exit 1
      [ "$STEPS_JOB_NAME" = release ] || exit 1
      [ -n "$STEPS_PIPELINE_NAME" ] || exit 1
      [ ${#STEPS_PIPELINE_REVISION} -eq 64 ] || exit 1
      [ "${STEPS_URL+set}" != set ] || exit 1
      echo "built by run $STEPS_RUN_ID"
    assert:
      stdout: built by run
  assert:
    execution: [stamp]
    outcome: succeeded
```

- **Unset, never empty.** A variable with nothing to say is absent, so test with `${VAR+set}`. `STEPS_URL` is unset under `steps run`/`steps test`, and under `steps web` when `--listen` is a wildcard address (`0.0.0.0`, `::`, `:8088`) with no `--external-url`.
- **A run link** is `$STEPS_URL/p/$STEPS_PIPELINE_NAME/runs/$STEPS_RUN_ID`.
- **Never hashed.** A run id changes every run; keying on it would mean a cached step never hits.
- **A cached task or get replays the first run's output**, so an artifact embedding the run id keeps the id of the run that built it. Read the variables in the `put` or hook that publishes: puts are never cached.
- **Not set for**: `check:` — including a `get`'s version check — because a check is not part of a build and a run id in a version would mint a new version every run; stdio MCP servers, which have no run and outlive commands; and expr/MCP-backed resources, which have no child process.
- **Not nameable in `env:`.** Listing any of the five names is a load error, rather than steps quietly replacing the operator's value. (`STEPS_WORKER`, the placement fact, is separate.)

## Downstream triggers (`trigger: true` + `steps web`)

By default `steps` is a one-shot, single-job CLI. `steps web` adds a long-running mode: it holds the pipelines uploaded to it by `steps pipeline set`, polls every resource named by any `get ..., trigger: true` step across every job of each one, and automatically runs whichever jobs are affected when that resource's latest version changes — including a version produced by another job's own `put`:

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
    trigger: true      # steps web runs this job when publish lands a new version
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
steps web --interval 30s --max-concurrent 1
steps pipeline set -c pipeline.yml
```

- **Two independent loops, connected only through a durable store-backed queue**: a **poller** checks every trigger resource on `--interval`, diffs the latest version against what's recorded, and enqueues every affected job; a **worker pool** (`--max-concurrent`, default 1) drains that queue by calling the same job runner `steps run` uses. The durable queue means a crash mid-run doesn't lose pending work.
- **At-least-once, never at-most-once**: a resource's recorded version only advances *after* every affected job is durably enqueued. If a check errors or the process crashes mid-poll, the resource stays "dirty" and is retried next poll rather than silently dropped.
- **Cold start builds the newest, seeds the rest.** A resource checked for the first time records everything it reports, marks everything below the newest as already taken, and triggers once on the newest — so a fresh (or freshly lost) state database can't mass-re-run every job the moment `watch` starts, and can't sit silent forever either. Concourse builds the single version its first check reports; this is the same outcome for a check that reports a window.
- **Dedup, ordering, and per-job concurrency**: a resource going dirty twice before a worker claims the row enqueues its affected job once — but a job already running can still get a fresh pending row queued behind it, so a version change mid-run isn't dropped. Claiming respects the job's [`max_in_flight:`](#max_in_flight--how-many-builds-of-one-job-at-once) — unlimited when unset, forced to 1 by `serial:`/`serial_groups:`.
- **Graceful-shutdown carve-out**: a job interrupted by SIGINT/SIGTERM mid-run — one that is [`interruptible:`](#interruptible--what-a-shutdown-does-to-a-running-build), or that outlasted the shutdown's grace — is *not* marked failed: its row is left "running" and reset to "pending" on the next startup, recovering a hard crash and an interrupted shutdown the same way. A step's own `timeout:` expiring is not an interruption; that build failed.

## Step renaming (`resource:`)

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

A `put` step takes the same field, mirroring Concourse's `put.resource`: the **step name is the `put:` value** — what the terminal, `steps runs steps`, `assert.execution` and a `to:`/`verdicts:` target call it — and the **resource published to is `resource:`**. One resource published several times for different reasons gets a name per reason instead of one word repeated:

```yaml
resource_types:
- name: emoji
  config:
    check: echo '[]'
    out: |
      echo {{ .params.add | shellquote }}
      echo '{}'

resources:
- name: reaction
  type: emoji
  source: {}

jobs:
- name: triage
  plan:
  - put: acknowledge     # the step is "acknowledge"...
    resource: reaction   # ...the out: that runs is "reaction"'s
    params: {add: eyes}
  - put: answered
    resource: reaction
    params: {add: white_check_mark}
  assert:
    execution: [acknowledge, answered]   # recorded under the STEP name
    outcome: succeeded
```

The terminal line names both when they differ (`put: acknowledge (resource: reaction)`).

- **Triggers resolve by the underlying resource**: `steps web` polls the *resolved* resource once no matter how many aliases reference it.
- **Load-time**: `resource:` is valid only on `get` and `put` steps and must name an existing resource. A `put:` naming no resource, with no `resource:`, says so and points at `resource:`.
- **Recording differs by kind**: an aliased `get` is recorded under the resource name (`execution: [repo, show]` above), a renamed `put` under its step name. A put records no resource version either way; what it publishes is the resource's `out:` result.
- **Caching**: an unaliased `get` or `put` hashes byte-identically to before this feature; an aliased `get` folds the artifact name into its hash, and a `put` whose name differs from its resource folds the name, so two otherwise-identical puts are two rows.

## Circuit breaker: `max_consecutive_failures:`

`steps web` runs unattended, and a job that fails on every new version will keep firing on every new version — burning model spend on a failure no automatic retry is going to fix:

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
Sun 02:00  nightly-summary HELD after 3 consecutive failures — release with: steps jobs release nightly-summary -p <pipeline>
```

- **It counts triggered RUNS, not the `attempts:` retries inside one** — conflating them would trip the breaker on ordinary flakiness a retry would have absorbed.
- **Consecutive, not cumulative.** A job that fails, passes, then fails is flaky, not broken. Any success resets the count.
- **Tripping is loud**: a `web.job_held` log record, and the job reads `⊘ held` on the jobs board and its own page.
- **An interrupted run does not count.** Ctrl-C is an operator, not a broken job.
- **Release is manual, deliberately** (`steps jobs release <job> -p <pipeline>`, or **Release** on the job's page). Not *resume*: that word is `--resume <run>`'s, continuing a failed run. Any successful run clears the breaker — including a manual `steps run`, the natural way to confirm a fix.
- **It holds back automatic triggers only.** A new version or a webhook delivery is skipped while the job is held; a person pressing Trigger in the web UI gets a run, since that is how somebody tries a fix from there. Unattended auto-resume would defeat the safety purpose.
- **Off by default.** A job that declares no ceiling never pauses; the count is still kept, so turning a breaker on later starts from a real number.

## How much history to keep: `run_history:`

`steps web` runs for weeks, and every build it does writes a run row, an event per step, what each agent step spent, the full text of every agent conversation, and a cached node per step. None of that used to be cleaned up. Measured on a pipeline answering Slack mentions overnight, one build cost about **23KB** — so a hundred builds a day added a couple of megabytes a day, forever, and three quarters of it was cached nodes and agent transcripts.

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
- **Default 100**, or `--run-history` on `steps run`/`steps web`. The pipeline wins when both are set, because it is the thing that knows how much its jobs write.
- **`0` keeps everything**, the same convention [every other limit here uses](attempts-timeout.md#zero-means-no-limit) — including `version_history:`. That is a real choice with a real cost, not a safe default.
- **A reaped run takes its whole story with it** — events, per-step records, agent spend and transcripts — so the run pages and `steps runs cost` reach back exactly as far as the cap.
- **Recorded configurations go with them too.** Each distinct pipeline a run was started from is stored once (that is the `CONFIG` column and the [`…/config/:sha` page](web.md)), and one no surviving run points at is reclaimed. It has no cap of its own and needs none: reachability is the bound. Two things follow from that rather than from `run_history:`. A configuration nothing ever ran under — every save an operator makes while `steps web` watches the file — is reclaimed at the next swap, so an afternoon of editing costs one row rather than one per save. And `0` here, which means no limit on *runs*, does not mean unbounded configurations: the sweep still runs, and what it keeps is what a surviving run says it executed. The newest is always kept, because it is the one the next run will name.
- **It also bounds the step cache**, but by COUNT rather than by age — cached steps are capped at a generous multiple of this number and the oldest go first. The distinction matters: a pipeline that is fully cached does no new work, so it adds no new cache entries, so nothing is ever evicted from it. Eviction happens only while new entries are being made, which is when the old ones are going stale anyway. What losing one costs is a re-run of a step whose content had not changed; nothing is recomputed *wrongly*.
- **What is NOT trimmed is what would change behavior.** Recorded resource versions have their own separate limit (`version_history:`), the per-job version cursor is never touched, and the chain-level "this content already succeeded" index is aged out on the same horizon rather than tied to node retention — so bounding history never re-answers a version a job has already handled, and never re-triggers.
- **An agent transcript is capped on its own too**, at 256KB, regardless of this setting: a conversation has as many turns as it needs, and one very long step should not be able to outweigh every other row in the database.

## `passed:` — only run against versions that are green upstream

Without it, `steps web` will trigger `deploy` on a commit the `test` job **already failed on**, and there is no way to say otherwise. This is a correctness gap, not a convenience:

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

- **A job records the versions it fetched or put only when the whole job succeeds.** `passed:` means "that job ran green against this exact version"; a job that failed after its `get` proves nothing about what it fetched.
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
- **Load-time checks.** `passed:` is get-only, may not name its own job, and may not name a job that neither gets nor puts the same resource — that last one would be a deadlock spelled as a typo. A `put:` only in a job-level `on_failure`/`on_error`/`on_abort` hook does not count: those run only when the build is not green, so nothing they publish can pass.

### A version a job put counts as having passed it

A `put:` records the version it prints against the build, exactly as a `get:` does, so a later job can consume what an upstream job published. The check never has to report that version:

```yaml
defaults:
  preflight:
    disabled: true

resource_types:
- name: registry
  config:
    check: |
      echo '[{"digest":"from-check"}]'
    in: echo {{ .version.digest | shellquote }} > digest.txt
    out: |
      echo '{"digest":"sha-1"}'

resources:
- name: toolchain-image
  type: registry
  source: {}

jobs:
- name: build-toolchain
  plan:
  - put: toolchain-image
  assert:
    execution: [toolchain-image]
    outcome: succeeded

- name: implement
  plan:
  - get: toolchain-image
    passed: [build-toolchain]      # the digest build-toolchain published
  - task: use
    inputs: [toolchain-image]
    run: cat toolchain-image/digest.txt
    assert:
      stdout: sha-1
  assert:
    execution: [toolchain-image, use]
    outcome: succeeded

assert:
  execution: [build-toolchain, implement]
```

- A put's version counts from the moment the build is green, even if the check never reports it. A build that put and then failed passes nothing.
- Every version a build fetched or put counts, several per resource allowed; the last one put is the newest.
- Nothing printed (or printing that is not a JSON object) records nothing, and a version over 4 KiB is printed with a warning and not recorded, so the gate stays shut.
- A `--resume`d run does not re-record a put an earlier attempt ran: the run prints a line saying so, and the gate stays shut rather than opening on a guess.
- A gated `get` with nothing green yet fails with `no version of <resource> has passed [<jobs>] yet`.

## `max_in_flight:` — how many builds of one job at once

By default a job's builds are **unlimited**, bounded only by `steps web --max-concurrent`. Cap it per job when the work is not safe to overlap but does not need full serialization:

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

`steps web --max-concurrent 4` runs jobs concurrently. For anything that deploys, publishes, or otherwise mutates the outside world, that is a hazard:

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
- **Membership is synced from the pipeline on every `steps web` startup.** A group removed from the YAML stops holding a lock immediately.

## `interruptible:` — what a shutdown does to a running build

`steps web` gets SIGTERM (a restart, a redeploy, a machine going down) while a job is mid-deploy. Whether that build is allowed to finish is the question this answers:

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

- **The default is to wait**, matching Concourse. Half-applying a deploy because someone restarted the daemon is the failure this exists to prevent.
- **The wait is bounded** (10 minutes). A job needing longer should carry its own `timeout:`, which still applies. The daemon logs `web.job.shutdown_wait` for each build it is waiting on; one still running when the grace ends is cancelled and re-queued like an interruptible one.
- **`interruptible: true`** is cancelled the moment the daemon is told to stop. Its queue row stays `running`, so the next startup re-queues it — nothing is lost, it is just re-run.
- **Only a shutdown waits.** Destroying a pipeline cancels its running build at once, whatever this field says, and so does renaming one — which re-queues the build under the new name.
- **This affects `steps web` only.** `steps run` is a person at a terminal, and ctrl-C there is always immediate.

## Webhooks

`steps web` polls on an interval. For a service that sends webhooks, a `type: webhook` resource skips the poll entirely: each delivery the daemon receives **is** a version, recorded before the sender is answered, and a job that gets it reads the payload. See [webhooks.md](webhooks.md) for the providers, filtering, and how to expose only the delivery route through a tunnel.
