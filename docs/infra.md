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

- **The step's tree goes with it, one artifact at a time.** Its declared `inputs:` are sent to the worker, the command runs there, and its declared `outputs:` come back — before the step's own `assert:` reads them. Each input or output travels as its own content-keyed piece and **a worker keeps what it receives**, so the second step of a job that shares an input sends nothing for it. That holds on both planes: through the artifact store, and over the tunnel, where the worker is asked whether it needs each piece before any bytes move. Nothing else travels: a large input does not get shipped home again to prove it did not change. The transfers are zstd-compressed when both ends speak it — negotiated at the handshake, so a shim built before compression existed gets the same trees uncompressed rather than a stream it cannot decode.
- **`STEPS_WORKER` names the tag** inside a placed command, and is unset for a step running locally. It is how a script tells the difference, and how the example above proves the tag took effect.
- **One tag.** Concourse intersects a step's tags against a pool of workers advertising theirs; there is no pool here, so a second tag would name a second machine and the step would have no home.
- **An unmapped tag is an error before the run starts**, not a fall back to local execution. A step that says it needs a GPU box, quietly running on a laptop, is the same broken promise `network:` without `image:` is refused for.
- **`local:`** runs the step through a shim in a child process on this machine — for trying a tagged pipeline out without a worker, and for debugging the shim itself: `--worker gpu=local:`.
- Valid on **task, get and put steps**, and on a **resource** (below). Invalid on `agent` steps and on a task with `fix:` — an agent's tools and conversation run in the orchestrator, so only its shell commands would move, leaving half a step on each machine — and, for the same reason, on a get or put whose resource type is `mcp:`- or `expr:`-backed, whose in and out do their file writing inside this process.
- **`image:` composes with `tags:`**: the step's tree is sent to the worker as usual and its command runs in a container **on that worker**, against the worker's own docker daemon. steps drives that daemon with its own client through a socket forwarded over the session's existing connection — no second port, and the worker needs no steps-specific container logic. The bind mount names the copy of the tree the worker holds, because a daemon resolves `-v` against its own filesystem. Two consequences worth knowing:
  - **The daemon that must exist is the worker's**, so the up-front `docker info` check cannot cover it: a machine acquired for the job does not exist when planning happens. A worker with no daemon fails the first placed step that names an image, in the daemon's own words, rather than at load time.
  - **`user:` is resolved on the worker.** An explicit `user:` crosses verbatim, as it does for a local container — it is a name the far end resolves, which is also how Concourse treats it. What changes is the *default*: unset takes the identity the **shim** runs as on a Linux worker, because the tree the container writes into lives there, so the ownership mismatch the default exists to prevent happens there too. Taking the orchestrator's own uid would answer about a different machine — a Linux orchestrator against a root shim asks for `--user 1000:1000` over a root-owned directory it cannot read. A worker that cannot report an identity, or one that is not Linux, defers to the image.
  - **The worker's tree must be somewhere its daemon can bind-mount.** This is the same `TMPDIR` constraint described above for local containers, one machine further away — a path the daemon cannot see is silently mounted as an *empty* directory, so the step succeeds and produces nothing. Name a disk in the worker URL (`aws://i-0abc123/var/tmp/steps`) rather than relying on the worker's temp directory.
- The remote contract is **sshd and a pushed `steps` binary**, nothing else — no agent to install, no daemon to run. `steps` uploads itself over SFTP on first use, keyed by the binary's own content hash, and reuses it after that.
- **The URL's path chooses a disk** on the worker: `ssh://jt@gpu-box/mnt/fast` puts both the pushed binary and the step's tree under `/mnt/fast`. Absolute, as written. Omit it and the worker's own temp directory is used, and `/tmp` is the wrong disk in two different ways. Where it shares the root filesystem, a build tree fills it. Where systemd mounts it as **tmpfs** — the default on Amazon Linux 2023, on Fedora, and on recent Debian and Ubuntu releases — it is *memory*, capped near half the machine's RAM: the pushed binary and the step's tree are then competing with the build for the same resource, and neither survives a reboot, so an acquired worker re-downloads the binary every time it is started. Measured on a `t4g.small`: a 921M tmpfs with 111M of it spent on the shim binary before the step ran. Name a path on a real disk — `aws://i-0abc123def456789/var/tmp/steps` — for anything past a first try.

  **steps says so rather than leaving you to find out.** The shim reports the filesystem its workdir sits on at the handshake — only that end can see it — and a `tmpfs` or `ramfs` workdir prints a warning naming the path, the type and the free space. A shim too old to say reports nothing, which is treated as *unknown* and never as "an ordinary disk": a warning on silence is one operators learn to scroll past.
- **Authentication** is your SSH agent, or `?identity=/path/to/key` for a specific key (an encrypted key has to go through the agent). Host keys are checked against `~/.ssh/known_hosts`, or `?known_hosts=` — never skipped.
- **A machine with no `known_hosts` entry** — one acquired on demand, used, and destroyed — is pinned by fingerprint instead: `?hostkey=SHA256:...`, exactly as `ssh-keygen -lf` prints it. Naming both it and `?known_hosts=` is an error rather than a precedence rule. A malformed fingerprint is refused when the mapping is read, not when the worker is dialed: a typo that only failed on connection is indistinguishable from the machine having been replaced, which is the alarm the pin exists to raise.
- **`~/.ssh/config` fills in what the mapping leaves out**, so a worker can be named the way you already name that machine: `--worker gpu=ssh://gpu-box` takes `HostName`, `User`, `Port`, `IdentityFile` and `UserKnownHostsFile` from the alias's own entry, `Include`s and all — the alias matched the way `ssh` matches it, lowercased first — with `%h`/`%p`/`%r`/`%d` and a leading `~/` expanded the way `ssh` expands them and a `UserKnownHostsFile` naming several files read as several files. Explicit beats ambient — anything written in the worker URL wins, and the file only answers what the URL did not. A key the URL names and `steps` cannot read is an error; one the *file* names is a candidate, so an absent or encrypted `IdentityFile` in a `Host *` block is skipped rather than fatal, exactly as `ssh` skips it, and so is a `UserKnownHostsFile` that does not exist yet — which leaves the host unknown, never unchecked. Point somewhere else with `?ssh_config=/path/to/config`, or read no file at all with `?ssh_config=none` — the spelling `ssh -F none` uses. The user file only: `/etc/ssh/ssh_config` is not read.
- **A directive outside that subset is refused, by name, rather than skipped.** `ProxyJump`, `ProxyCommand`, `ProxyUseFdpass`, `CanonicalizeHostname` and `HostKeyAlias` are not implemented, and an alias resolved for its `HostName` and then dialed *directly* would run the step on a machine you did not authorize — silently, wherever the direct route happens to work. Reading a config file partially is not the same as not reading it. Their off switches are honored rather than refused, since `ProxyJump none` under a `Host *` that sets one is a host saying dial me directly. `UserKnownHostsFile none` — and its `/dev/null` spelling of the same intent — is refused for the same reason a worker's host key is never skipped — unless `?hostkey=` already pins the key, which answers that question outright. Every one of these says `?ssh_config=none`, which dials the mapping exactly as written.
- **`Match` is understood only as `Match host` and `Match all`, matched against the alias.** A block written on any other criterion — `exec`, `user`, `localuser`, `final`, `canonical`, `tagged` — makes the whole file unreadable here, and the worker is refused rather than dialed from a file half of which was skipped; `Match exec` would mean running a command to decide, which reading a config file does not get to do. A block this *can* read is still only approximately resolved, so it is refused whenever it names a directive in the subset, `Include`s of its own included — tolerable for a block about agent forwarding, and not for one that decides which machine gets the step.
- **An EC2 instance is reached with `aws://i-0abc123def456789`**, through SSM rather than SSH: no inbound port, no sshd, no host key, and no public address — the instance's own agent dials out to the AWS control plane, which is what makes a worker behind NAT reachable at all. steps sends a bootstrap command that starts `steps _shim` listening on **loopback** and serves exactly one session, then opens a port-forwarding session to it; nothing is left running when the step ends. Serving one session only ends a shim somebody reached, so the bootstrap also gives it a **linger bound**: a shim that is never dialled — the session throttled, the websocket refused, the orchestrator killed in between — reaps itself instead of sitting in `accept()` holding a port. The bound covers the wait for the first connection and never the step, which may legitimately run for hours. A machine EC2 already calls *running* has no registered SSM agent for another one to three minutes, so the dial waits for the agent rather than failing on the first ask — which is what an acquired worker spends most of its acquisition on. Auth is IAM (`ssm:StartSession`, `ssm:SendCommand`, `ssm:GetCommandInvocation`, `ssm:DescribeInstanceInformation` — and the `ec2:` Start/Stop/CreateFleet/Terminate/Describe actions if the acquisition rungs below are used), and the instance profile needs `AmazonSSMManagedInstanceCore` and nothing more — artifact bytes reach it through presigned URLs, so it holds no AWS credentials of its own. `?region=` overrides the ambient region; the URL's path chooses a disk, as it does for `ssh://`.
- **An `aws://` worker needs a shim binary built for its platform**, since steps cannot cross-compile one in the field: either `?binary=/path/to/steps-linux-amd64`, a **local** binary uploaded to the artifact store once (keyed by its content hash) and fetched by the bootstrap from a 20-minute presigned URL — which is why `?binary=` there requires `--artifact-store`, checked before the run starts rather than after a machine has been acquired — or `?shim=/usr/local/bin/steps`, naming a binary **already on the instance** for an AMI that bakes one in, where nothing is transferred and no store is needed. Naming both is an error rather than a precedence rule. A Windows instance is refused before anything is fetched or started, for the same executable-bit reason a Windows worker is refused anywhere else.
- **A worker can be acquired for the job rather than named**, on two further rungs of the same `aws://` scheme: `aws://stopped/i-0abc123def456789` starts a **parked** instance, uses it, and stops it again; `aws://launch/lt-0def4567890abcde` **launches** one from a launch template and terminates it when the job ends. The launch template owns the entire EC2 vocabulary — AMI, instance type and overrides, subnet, security groups, instance profile, user data — so steps adds no EC2 configuration surface of its own.
- **Acquisition is per JOB, never per step.** Cloud acquisition costs 20–90 seconds and real money, so the first placed step in a job pays for the machine, every later step reuses it, and the job's end gives it back — however the job ends, including cancelled. A job whose placed steps are all cache hits acquires nothing at all. A parked worker is parked again as soon as the job ends. `?idle=` holds it running for that long first — the releasing job visibly waits out the window, which is the honest price of the next job finding the machine warm — and an unknown or misplaced worker option (`?capactiy=`, `?idle=` off the stopped rung) is refused when the mapping is read, since a typo on the knob that decides what a machine costs must not be silently ignored.
- **`?capacity=spot|spot-then-od|od`** chooses what a launched worker asks for — **default: `od`**, stated explicitly in the request, because AWS has no "template decides" semantics for a fleet's capacity type and the knob that decides what a machine costs must not default somewhere silently. Spot uses `price-capacity-optimized` allocation, and `spot-then-od` asks for both in one request so a busy pool costs money rather than a failed job. A pool with no capacity fails with EC2's own account of why.
- **`?version=` chooses which launch-template version a launched worker is built from** — `$Default` (the default when unstated), `$Latest`, or a version number like `3`. Bare `default` and `latest` spell the first two without the `$` an unquoted shell eats, and `?version=` with no value is refused rather than read as "default", since that empty value is exactly what `?version=$Latest` degrades into. Refused off the launch rung, where there is no template to have versions.

  A launch template is not one editable blob: it is a container of **numbered, immutable versions**, each holding a complete machine shape — AMI, instance type, `BlockDeviceMappings`, subnet, security groups, instance profile, user data. `aws ec2 create-launch-template-version --source-version 1` appends a new one from a delta. **This is the whole of steps' machine-shape surface, and deliberately so**: a job that needs a 200GB disk or a bigger instance type names a version that has one, rather than steps growing a `?disk=` and an `?instance_type=` and, eventually, a second-rate copy of the EC2 API in a URL. Note that `?version=` is inert on the other two rungs for the same reason it is refused there — they name a machine whose shape was decided when it was created.

- **A Compute Engine instance is reached with `gcp://worker-1?project=my-proj&zone=us-central1-a`**, through IAP TCP forwarding to the instance's own sshd. GCP has no SSM-shaped exec channel, so the SSH contract is the transport — the relay tunnel just carries it: the client opens an outbound websocket to Google's relay, and the relay's own range (`35.235.240.0/20`) reaches port 22 on the instance's VPC interface. The honest comparison with `aws://`: **no public address and no internet-reachable port**, but the instance does run sshd and its firewall must admit that one Google-owned range to 22 — one rule, not zero. Authentication is minted, not configured: the orchestrator process generates an ephemeral key, installs its public half through instance metadata in the expiring `google-ssh` form (the guest agent stops honoring it after 12 hours, and steps prunes expired entries whenever it installs a fresh one), and verifies the host against the SSH host keys the instance itself published to **guest attributes** — which is what makes a machine created moments ago verifiable at all. The instance's template (or metadata) must set `enable-guest-attributes=TRUE`; `?hostkey=` pins the key instead for an image where it cannot. `?project=` and `?zone=` locate the instance, falling back to `GOOGLE_CLOUD_PROJECT`/`CLOUDSDK_*` and the ADC credentials' own project; auth is Application Default Credentials (`gcloud auth application-default login`), and the caller needs `roles/iap.tunnelResourceAccessor` plus `roles/compute.instanceAdmin.v1`. The URL's path chooses a disk, as everywhere. Because sftp carries the binary, **`?binary=` needs no artifact store on `gcp://`** — and a non-Linux orchestrator must supply one, checked before the run starts.
- **The same two acquisition rungs exist**: `gcp://stopped/worker-1` starts a parked instance and stops it after the job (`?idle=` holds it warm, exactly as on AWS — note a stopped GCE instance reports the status `TERMINATED`, Compute Engine's word for parked); `gcp://launch/template-1` creates an instance from an **instance template** and deletes it when the job ends. The template owns the entire machine vocabulary — image, machine type, disks, network, service account, provisioning model, metadata — and two `aws://` knobs deliberately do not exist here: **no `?capacity=`**, because a GCE template *decides* its own provisioning model (a spot job names a spot template — set `instanceTerminationAction: DELETE` in it, or a preempted worker's disk keeps billing), where an EC2 fleet request cannot; and **no `?version=`**, because an instance template is one immutable object rather than a container of numbered versions — a different shape is a different template.
- **A preempted GCE worker drains exactly as a spot EC2 one does.** The shim watches the instance metadata service — both clouds answer the same link-local address, and the shim tells them apart once per session — and relays either of GCE's two ways of saying it: the `preempted` flag flipping to TRUE (a market preemption), or a `maintenance-event` of TERMINATE_ON_HOST_MAINTENANCE (how `instances.simulateMaintenanceEvent` announces itself on a spot instance — measured: it never touches `preempted`). A MIGRATE maintenance event is deliberately not a notice, since a live migration is a machine the worker keeps. The warning is shorter (about thirty seconds against EC2's two minutes) and there is no rebalance-recommendation analog, so every GCE notice is terminal.
- **A worker on a different OS or architecture** needs a binary built for it: `?binary=/path/to/steps-linux-arm64`. `steps` has no Go toolchain in the field, so it cannot cross-compile one for you; `CGO_ENABLED=0` is what makes the one you build pushable.
- **A Windows worker is refused**, at the handshake, before any tree is sent. A tree crosses the wire carrying each file mode, but Windows has nothing to store `0111` in: `os.Chmod` there sets only the read-only attribute and returns no error, and `os.Stat` reports every regular file as `0666` (or `0444`). Left to run, the tree would unpack without its executable bits, the repack on the way home would read that back off the filesystem, and what returned would be a tree whose scripts are no longer scripts — with the step cache seeing content that changed for no reason a reader can point at, a later step unable to run the file, and no error anywhere. Refusing is the same call the codec makes about a fifo, for the same reason: a cache that quietly disagrees with itself is worse than a step that refuses to ship. This is a property of the worker's *filesystem*, not its CPU, so no architecture is refused for being one — but nor is a Linux or macOS worker safe by virtue of its OS, which is what the next point measures. A shim that reports no OS at all is accepted, since it has said nothing to refuse.
- **A worker whose *work directory* cannot hold an executable bit is refused too**, on the same terms and for the same reason — and this is the case the OS above does not cover. `ssh://user@host/mnt/c/scratch` on a WSL2 machine reports a perfectly ordinary Linux, while `/mnt/c` is DrvFs over NTFS and loses the bit exactly as Windows does; a `?root=` aimed at vfat or exfat (whose `fmask`/`dmask` synthesize a fixed mode), at CIFS without unix extensions, or at 9p/virtiofs under Lima or Docker Desktop answers the same way. Since the worker URL's path *is* the work directory, this is something a working flag can reach rather than an exotic machine. So the shim **measures** rather than infers: immediately after creating its work directory it writes a file there, sets `0700`, reads the mode back, and removes it — and reports the answer in the handshake. The measurement and the OS check are both consulted and either one refuses. A shim too old to have measured says nothing and is accepted, the same compatibility posture the build and OS checks take.
- **With `--artifact-store`, trees move through the store instead of the tunnel** — see [Artifact store](#artifact-store---artifact-store) below. Negotiated at the handshake like compression; without the flag, or against an older shim, the tunnel carries everything, as it always could.
- **A reclaimed worker is not a failed step.** A spot instance learns it is going away ahead of time — about two minutes on EC2, about thirty seconds on GCE — through instance metadata and nowhere else, so the shim watches for it and tells the orchestrator; a failure that follows is reported as an eviction rather than as the command's own verdict. If the worker is one steps **acquires** (the `stopped/` and `launch/` rungs of `aws://` or `gcp://`), the step is then re-placed on a freshly acquired machine — up to twice — **without spending the step's `attempts:` budget**, because `attempts:` is your statement about your own work and the cloud taking a machine is neither caused by it nor fixable by it. A worker that merely names a machine that already exists (`ssh://`, `aws://i-*`, `gcp://worker-1`) has nowhere else to go, so it is reported rather than retried against the host that just vanished. Re-placement is refused once the step's own `attempts:` x `timeout:` wall clock has been spent — a bound on when a fresh machine may be taken, so an eviction near the end of the budget can still finish one last round rather than being cut mid-promise.

  This is a deliberate divergence from Concourse, which errors the build when a worker vanishes. Two distinctions keep it honest. A command that ran and **chose** a nonzero status still failed — a machine disappearing afterwards does not unsay an answer already given — while a command the shutdown **signalled** is the machine ending it, not the command answering, and counts as infrastructure. And an EC2 *rebalance recommendation* is only advisory: it is reported and the worker is let go once the step finishes, but it never destroys a healthy machine the way a real reclamation does.
- **A worker that dies mid-step is redialed on the next try.** A crashed machine or a dropped tunnel fails the running command as an *error* (never as the command's own verdict), and the step's next command — an `attempts:` retry, typically — opens a fresh connection and re-sends the step's local tree, which already holds everything earlier commands fetched back. The boundary is the handshake: a connection that died any time after the worker answered its hello — even while the tree was still uploading — is redialed, while a host that could not be reached or never answered stays failed for the whole step, since the first answer is the true one and every re-ask would cost another timeout.
- **The run record says where each step ran.** A finished placed step carries `tag (address)` — in the web UI's step header and in `run_events` — and a step that ran locally carries nothing, so the rows that left stand out. The address only: `?identity=` and `?hostkey=` describe how to authenticate and are not written to the record. An alias is recorded as the alias, not as whatever `~/.ssh/config` resolved it to that day — the mapping is the stable name for the machine, and the resolution is a connection detail that can differ between the machines running `steps`. Nothing else can answer the question after the fact, since `tags:` is deliberately outside the hash.
- **And what that machine turned out to be.** `steps runs where <pipeline>` reports, per placed step, the tag, the platform the worker reported (`linux/arm64`), the filesystem its tree landed on and the free space left there, how many bytes had to be pushed to it, and the machine — plus the image, if the step ran in one. Add a run id for a specific run; without one, the newest. All of it comes from the worker's own handshake, because nothing on the orchestrator can see it, and it is the set of answers to *it passes on my laptop and fails on the fleet*. A worker that could not report a filesystem reads as `not reported` rather than as a blank that looks like an ordinary disk.

  The run page draws the same rows on a **machines** panel beside the spend one, with a `tmpfs` workdir marked in warning colour — see [web.md](web.md#the-transcript).

  **A tagged hook is reported too.** `on_failure:`, `ensure:` and their siblings really do acquire a machine — on `aws://launch/` that means launching and billing an instance — and it is the place an operator is least likely to expect one running. A hook is deliberately outside the merkle chain, so that a hook never gets skipped for having succeeded before; that means it has no cached node, and its row is identified by the hook's own scope (`step 0 (task "build") (on_failure hook)`) instead of a content hash.

  **Facts, never a price.** There is no cost column, deliberately: what an instance-hour actually cost is not knowable from inside a run — list prices ignore Savings Plans and Reserved Instances, a spot instance's paid price is reported by no API, and real billing lands up to a day later. A confident wrong number under `COST` is worse than no column, and anyone holding their own rate card can price these rows. This is the opposite call from `steps runs cost`, where the *provider* reports the dollars and steps only records what it was told (and shows nothing when it was told nothing).

- **Caching**: `tags:` does **not** fold into the node's hash. Placement decides where a step runs, not what it produces, and a tree that crossed the wire digests identically to one that never left — so retagging a step, or repointing a tag at a new machine, does not re-run work that already succeeded.

### Resources on workers

A source only reachable from a worker's network — a git host inside a VPC, a registry behind a bastion — has to be checked, fetched and pushed from there. `tags:` on the **resource** places all three:

```yaml test=infra-resource-worker
resource_types:
- name: probe
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
- **The fetched tree comes home.** A placed `in:` fills a directory on the worker and the whole of it is brought back, so a later step — here or on another worker — reads it exactly as it would a local fetch, and the resource cache digests it identically. The cost is that bytes travel worker → orchestrator → wherever the next step runs; there is no worker-to-worker streaming.
- **The poller places checks too — on a machine that already exists.** `steps web` polls with the same `--worker` mappings, and a polled resource's tag must name a standing worker (`ssh://`, `local:`, `aws://i-…`, `gcp://name`): an acquisition rung (`aws://launch/`, `aws://stopped/`, `gcp://` launch) is refused at startup, because a poll and a running job would each hold a lease on the same machine with nothing saying who owns it, and a check that runs once an interval is the wrong thing to launch a billed instance for. A job's own get/put may still name an acquirable rung — it pays for the machine per job, as a task does — but the pre-plan freshness check skips such a resource and resolves from recorded history rather than launching a machine for a run that may turn out fully cached. An unmapped resource tag is refused before anything is polled, and `steps plan` takes `--worker` for the same checks.
- **A version is text the worker wrote.** What a placed `check:` prints becomes `{{ .version.* }}` in the `in:` that runs next — on the step's worker, or on this machine when a step overrides the resource's tag with `local:` — so a version field crossing machines is exactly the untrusted value [templating.md](templating.md#shell-quoting-untrusted-values) says to pass through `shellquote`. The example above does.
- **Shell-backed types only.** An `mcp:` or `expr:` type's in and out write their files from inside this process, so a tag could move only a fraction of the stage; the load refuses it and says so.
- **A resource type's `image:` runs on the worker's daemon**, found the way `docker` finds it there — `DOCKER_HOST`, then the selected `docker context` — since an ssh session has no shell profile to set the variable. On a worker whose daemon runs in a VM (colima, Docker Desktop) the shim's workdir must be a path that VM shares, or the container's bind mount is silently empty and a placed `in:` fetches nothing: name the disk in the mapping, `--worker vpc=ssh://box/home/jt/steps`, the same way [`TMPDIR`](#tmpdir-when-the-daemon-runs-in-a-vm) fixes it locally.

### The life of an `aws://` worker

Three things happen on three different clocks, and knowing which is which explains most of the behaviour above. A **placed step** is one carrying `tags:`; a **shim** is the `steps` binary running in worker mode on the far end; a **rung** is which of the three `aws://` forms you wrote.

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
│   ├─► BOOTSTRAP ── once per CONNECTION (SSM SendCommand)
│   │     fetch the binary from a 20-minute presigned URL, unless
│   │       <root>/steps-shim/<content-hash>/steps is already there
│   │     start  steps _shim --listen 127.0.0.1:0 --once
│   │
│   └─► CONNECT ── SSM port-forward session to that loopback port
│         inputs over, command runs on the host, outputs back
│         the shim serves ONE connection and exits
│
├─ later placed steps: reuse the machine, fresh --once shim each time
│    (a step that redials after a dropped tunnel bootstraps again too)
│
└─ job ends (succeeded, failed, or cancelled)
    │
    └─► RELEASE ── once per JOB
          aws://i-0abc...          nothing; the machine is yours
          aws://stopped/i-0abc...  StopInstances, after ?idle= if set
          aws://launch/lt-0def...  TerminateInstances
```

What that leaves on the instance between steps: the cached binary under `<root>/steps-shim/<content-hash>/`, and nothing else. No steps process runs, because `--once` means the shim exits after the one connection it was started for.

What runs the command: the worker's host, unless the step names an `image:`, in which case the worker's own docker daemon runs it in a container against the copy of the tree the shim unpacked. A stock Amazon Linux 2023 AMI has no daemon, so an `aws://` worker needs one installed — through the launch template's user data, or baked into the AMI — for placed steps that name an image, and needs nothing at all for those that do not.

What the instance never holds: AWS credentials. Artifact bytes arrive over presigned URLs the orchestrator mints per transfer, so the instance profile needs `AmazonSSMManagedInstanceCore` and nothing else — not even read access to the bucket its own inputs came from.

**A `gcp://` worker lives the same life on different machinery.** ACQUIRE is `instances.insert` from the template (or `instances.start` for the parked rung); there is no separate BOOTSTRAP errand, because the IAP tunnel terminates at sshd and the binary rides in over sftp exactly as `ssh://` pushes it — cached on the worker by content hash the same way; CONNECT is the relay websocket carrying the SSH session; RELEASE is `instances.delete` (or `stop`). What a GCE instance never holds: GCP credentials of its own for steps' purposes — the orchestrator's ADC signs the tunnel, and artifact bytes still arrive over presigned URLs when a store is configured. One machine-shape note with money attached: name a spot provisioning model **with `instanceTerminationAction: DELETE`** in the template, or every preempted worker leaves a stopped instance whose disk keeps billing.

## Artifact store (`--artifact-store`)

The step cache's remote half: cached step outputs mirrored to a content-addressed store on S3, so bytes evicted locally — or never present on this machine — are materialized back instead of re-earned by running the step. Opt-in, and CLI-only by design: the flag names infrastructure, and pipelines stay portable.

```console
steps run --artifact-store s3://my-bucket/team-prefix pipeline.yml
```

- **The flag has two consumers.** With a durable `workspace.root:`, cached step outputs are mirrored to the store — the fleet cache. With `--worker` mappings, placed steps use the store as their **data plane**: trees move by presigned URL and the SSH tunnel carries control frames only, which is what makes a slow tunnel irrelevant to a large tree. Either works without the other; a run with no durable root logs that the mirror half is off rather than refusing.
- **A worker needs zero AWS identity.** The orchestrator mints short-lived presigned GET/PUT URLs per transfer, so a worker can touch exactly the objects its job was handed and nothing else — its only requirement is HTTPS egress to the bucket. A worker that cannot reach the store fails the step with the store's answer in the error; a worker whose `steps` binary predates the data plane silently gets the tunnel, which is always the floor.
- **Step trees travel one ARTIFACT at a time**, each content-keyed under `<prefix>/wire/`, and **a worker keeps what it fetches**. Two steps of one job share their inputs and differ only in their outputs, so a whole-tree key never repeats — measured, a 64MB input through three placed steps moved 192MB to the worker with the store deduplicating none of it, because the trees differ by an empty directory and hash differently. Named per artifact, the shared input crosses once. The worker's cache is bounded by size and evicted coldest-first; a redial after a worker died, or a spot replacement, finds both the store object and the worker's own copy already there. Fetch objects are one-shot and deleted after use; a lifecycle rule on `wire/` is a safe backstop for what a crash leaves behind.
- **The bucket holds bytes, never truth.** Each output travels as one zstd tarball keyed by its content digest, under `<prefix>/blobs/<digest>`. Which digest means what — the mapping from "this work over these input bytes" to "these output digests" — stays in the pipeline's own state database. That split is what makes an S3 lifecycle rule expiring untouched objects always safe: the worst case is a re-upload, never a wrong skip.
- **A machine handed the state file inherits the cache.** A fresh checkout with the `.steps/<name>.db` and the same flag skips the steps the database says succeeded, fetching their outputs by digest — which is what makes a CI runner that persists only the small state file, not the workspace, warm on its second run.
- **What arrives is verified, not trusted**: every fetched tree is re-digested before it is installed, and bytes that do not match their key are refused as an ordinary miss. Every mirror failure — store unreachable, blob expired, index unknown — costs a re-run and nothing else; no mirror failure can fail a build that is otherwise working.
- **Uploads are skipped when the store already holds the digest** (one `HEAD` per output), so an unchanged output is never re-shipped, whoever produced it first.
- **Credentials and region** come from the ambient AWS configuration — the same chain every AWS tool reads — with `?region=` as an override. `?endpoint=` points at an S3-compatible server that is not AWS (minio and friends), switching to path-style addressing.
- Applies to `run`, `watch`, `test`, and `web`. `volatile:` steps are never cached, so they are never mirrored either.

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
- **Works on both execution paths.** On the host the named variables are added to the allowlist; in a container their values are sent to the daemon in the request that creates it, so the secret never appears in an argument vector the host's process list would expose.
- **An unset variable contributes nothing** rather than an empty value, so a command can still tell "not configured" from "configured empty" — with the colon-less shell forms (`${VAR+set}`, as above, or `${VAR-fallback}`); `${VAR:-fallback}` collapses the two.
- **Step-level override**: a `task`/`agent` step's `env:` replaces the referenced entry's for that step only. Unlike `image:` this is *declared*-wins, not non-empty-wins — an explicit `env: []` means "nothing beyond the baseline", which is a real thing to want. Invalid on `get`/`put` steps (set it on the resource type).
- **Caching**: the variable **names** fold into the node's hash. The values do not — a value changing is the operator's environment moving under the pipeline, which steps has never claimed to hash.

## Downstream triggers (`trigger: true` + `steps web`)

By default `steps` is a one-shot, single-job CLI. `steps web pipeline.yml` adds a long-running mode that polls every resource named by any `get ..., trigger: true` step, across every job in the pipeline, and automatically runs whichever jobs are affected when that resource's latest version changes — including a version produced by another job's own `put`:

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
steps web pipeline.yml --interval 30s --max-concurrent 1
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

- **Triggers resolve by the underlying resource**: `steps web` polls the *resolved* resource once no matter how many aliases reference it.
- **Load-time**: `resource:` is valid only on `get` steps and must name an existing resource.
- **Caching**: an unaliased `get` hashes byte-identically to before this feature; an aliased `get` folds the artifact name into its hash.

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
Sun 02:00  nightly-summary PAUSED after 3 consecutive failures — resume with: steps jobs resume <pipeline> nightly-summary
```

- **It counts triggered RUNS, not the `attempts:` retries inside one** — conflating them would trip the breaker on ordinary flakiness a retry would have absorbed.
- **Consecutive, not cumulative.** A job that fails, passes, then fails is flaky, not broken. Any success resets the count.
- **Tripping is loud**: a printed line plus a `trigger.job_paused` log record.
- **An interrupted run does not count.** Ctrl-C is an operator, not a broken job.
- **Resume is manual, deliberately** (`steps jobs resume <pipeline> <job>`). Any successful run clears the breaker — including a manual `steps run`, the natural way to confirm a fix. Unattended auto-resume would defeat the safety purpose.
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

- **The default is to wait**, matching Concourse. Half-applying a deploy because someone restarted the watcher is the failure this exists to prevent.
- **The wait is bounded** (10 minutes). A job needing longer should carry its own `timeout:`, which still applies.
- **`interruptible: true`** shares the watcher's context and is cancelled with it. Its queue row stays `running`, so the next startup re-queues it — nothing is lost, it is just re-run.
- **This affects `steps web` only.** `steps run` is a person at a terminal, and ctrl-C there is always immediate.

## Webhook-triggered checks

`steps web` polls on an interval: short means fast reaction and lots of API calls, long means slow reaction. A webhook removes the tradeoff — react instantly, poll rarely as a safety net:

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
steps web pipeline.yml --listen 0.0.0.0:8080 --read-only
curl -X POST 'http://localhost:8080/p/pipeline/check/repo?token=…'   # or: Authorization: Bearer …
```

The route lives under the pipeline it checks, on the same address the UI is
served from. The poll loop used to open a second port of its own, so a
deployment that wanted both had two addresses and two HTTP surfaces to expose;
one daemon means one listener. `pipeline` in the path is the pipeline's name —
the YAML's base name unless `--name` says otherwise, the same string as its
`/p/<name>/` page.

> **One listener means one exposure.** Reaching this endpoint from outside the
> machine means binding `--listen` to a routable address, and that address also
> serves the UI — which has **no authentication at all** (see
> [web.md](web.md#security)). Anyone who can reach the port can read every run,
> transcript and log, and — without `--read-only` — trigger any job, decide any
> approval, and answer any question. Pair the two flags, as above: `--read-only`
> withholds every browser control while leaving this token-authenticated route
> working, which is exactly the shape a webhook receiver wants. Put it behind a
> reverse proxy if the UI needs to be reachable too.

- **The token is a credential, not config.** `webhook_token_env:` names an environment variable, following `api_key_env:`. A literal is rejected at load — a resource's fields are hashed into the merkle content map, so a literal token would be written to `state.db` in cleartext.
- **An unset token variable accepts nothing.** Reading an empty expectation as "no auth required" would turn a deployment mistake into an open trigger endpoint.
- **A bad token and an unknown resource are indistinguishable** (both 401) — otherwise the endpoint is a free directory of a pipeline's resource names.
- **POST only.** A GET would be triggerable by a browser preview or a link scanner.
- **Exempt from the UI's same-origin check**, and only this route: a webhook sender is cross-origin by definition, and the resource's own token is the stronger check. Every browser mutation still requires a matching `Origin`.
- **A pipeline that names no `webhook_token_env:` resource has no route at all** — 404 rather than an endpoint that authenticates nothing. That is also the way to turn the endpoint off: `--read-only` does *not* withhold it, deliberately (see [web.md](web.md#triggering-approving-resuming)).
- **The poll loop keeps running.** A webhook that is never delivered must not mean a change is never noticed.
- **A webhook treats the version as changed** even when it matches what was last recorded: the sender knows something the check output may not show yet.
