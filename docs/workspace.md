# Workspace Isolation

Every step runs isolated: its working directory is materialized from its own declared `inputs:` and nothing else, and only its declared `outputs:` are captured back. A `get` fetches into the build's artifact store; a `task`/`agent`/`put` step sees an artifact only by naming it. There is no shared mutable directory and no way to opt into one — an artifact reaching a step that never declared it would be ambient data flow, and every flow in this DSL is opt-in.

```yaml
jobs:
- name: build
  plan:
  - task: generate
    outputs: [meta]                  # an empty meta/ exists; its content is captured
    run: echo 42 > meta/answer.txt
  - task: consume
    inputs: [meta]                   # sees meta/ because it says so — and nothing else
    run: cat meta/answer.txt
    assert:
      stdout: "42"                   # the artifact really crossed the step boundary
  - task: blind
    run: test ! -e meta              # declared nothing, sees nothing
    assert:
      code: 0                        # test(1) agrees meta/ is absent here
  assert:
    execution: [generate, consume, blind]
    outcome: succeeded
```

- **`inputs:`/`outputs:` are optional and default to empty.** An absent `inputs:` means the step mounts nothing; a pure-compute step legitimately declares nothing. An agent step's `dir:` also names the artifact it works in (its first path component) and is validated the same way.
- **Declared inputs are validated against producers.** An `inputs:` (or agent `dir:`) naming an artifact nothing earlier fetched or produced is a plan-time error — "this step reads an artifact nobody produced" fails before any command or model runs.
- **`put` steps compose a read view the same way** from their own `inputs:`. There is no implicit "all artifacts so far" view, but `inputs: all` on a put is the explicit escape hatch:

```yaml
resource_types:
- name: archive
  config:
    out: |
      ls a b            # the put's view really holds both artifacts

resources:
- name: bundle
  type: archive
  source: {}

jobs:
- name: publish
  plan:
  - task: one
    outputs: [a]
    run: echo 1 > a/one.txt
  - task: two
    outputs: [b]
    run: echo 2 > b/two.txt
  - put: bundle
    inputs: all         # every artifact produced so far; valid only on put steps
  assert:
    execution: [one, two, bundle]    # `ls a b` in out: would fail if either were missing
    outcome: succeeded
```

## Read-modify-write

A name in both `inputs:` and `outputs:` materializes the artifact's current content and captures it back over the artifact after the step succeeds. This is how a revise loop carries state between visits — the looping step declares the same artifact both ways and each visit continues from the last captured state:

```yaml
jobs:
- name: accumulate
  plan:
  - task: start
    outputs: [log]
    run: echo first > log/entries.txt
  - task: append
    inputs: [log]                     # starts from what `start` captured…
    outputs: [log]                    # …and its changes become the new content
    run: echo second >> log/entries.txt
  - task: show
    inputs: [log]
    run: wc -l < log/entries.txt
    assert:
      stdout: "2"                     # both entries survived — the read-modify-write held
  assert:
    execution: [start, append, show]
    outcome: succeeded
```

A step that *fails* captures nothing, so put the state-advancing write in a step that succeeds.

## Name mapping (task steps only)

`input_mapping:`/`output_mapping:` are `{task-config-name: plan-artifact-name}`, mirroring Concourse: a reusable `tasks:` entry with pinned input/output names can be pointed at whatever a job actually produced without editing the task. The directory on disk keeps the task-config name; the artifact copied in / captured out uses the plan name:

```yaml
tasks:
- name: count-lines            # a reusable task, written against "src"
  inputs: [src]
  outputs: [report]
  run: wc -l src/* > report/count.txt

jobs:
- name: audit
  plan:
  - task: fetch
    outputs: [handbook]
    run: printf 'a\nb\n' > handbook/pages.txt
  - task: count-lines
    input_mapping: { src: handbook }        # the plan's "handbook" appears as src/
    output_mapping: { report: audit-notes } # captured as "audit-notes"
  - task: check
    inputs: [audit-notes]
    run: cat audit-notes/count.txt
    assert:
      stdout: "2"                           # the mapped-in handbook is what got counted
  assert:
    execution: [fetch, count-lines, check]
    outcome: succeeded
```

Mapping keys must be a subset of the resolved task's declared inputs/outputs, and mapping values must be plain artifact names.

## Tuning materialization: the `workspace:` block

Optional tuning for *how* trees are materialized — not a switch that turns isolation on:

```yaml noexec=btrfs
workspace:
  strategy: btrfs       # default: copy. btrfs (Linux only) snapshots instead of copying
  root: /mnt/btrfs      # optional for copy (default: system temp); required for btrfs
  options:
    compression: zstd   # btrfs only: zstd | lzo | zlib | none

jobs:
- name: build
  plan:
  - task: hello
    run: echo hi
```

- Under `copy` each input is a copy; under `btrfs` it's an instant copy-on-write snapshot. Both produce the same logical view, so `strategy`/`root`/`options` are never hashed — switching backends invalidates no one's cache. The `inputs:`/`outputs:`/mapping declarations themselves **are** hashed unconditionally: they're the step's executed view, so changing them re-runs the step.
- This is corruption hygiene, not a sandbox: shell commands can still reach outside the materialized directory via absolute paths, same as always.
- The provider is built once per CLI invocation and validated at startup — wrong platform, wrong filesystem, missing binaries all fail fast, before any step runs.
- **Leftovers from a crashed run are swept at startup** (a SIGKILL'd process never removed its build directory; under btrfs those hold live subvolumes ordinary removal doesn't reclaim). The sweep only touches directories this provider created, is best-effort, and is skipped under `--keep-workspace` — that flag means "leave the files for me to look at".
- **`outputs:` on an `across:` step collects**: each cell's capture lands under the cell's own coordinates inside the declared artifact (`findings/alpha/…`) — see [control-flow.md](control-flow.md). Requires `strategy: copy` (the default).
- Fix agents (`fix:`) run inside the failing task's own already-materialized working directory, not a fresh one — they need the exact state the task failed in.

## Cross-build resource cache (`cache:`)

Agent and `put` steps make their chains unskippable, so a real run re-fetches every `get` from scratch — the network and disk paid again every time. The cache keeps fetched versions across builds:

```yaml noexec=btrfs
workspace:
  strategy: btrfs
  root: /mnt/btrfs
  cache:
    resources: true
    max_entries: 50    # optional; least-recently-used evicted first

jobs:
- name: build
  plan:
  - task: hello
    run: echo hi
```

- **Off by default, and the reason matters.** A cached version's `in:` does **not** run again. That is correct under the resource contract — the same version materializes the same content — but a resource type whose `in:` has side effects beyond writing the directory (bumping a counter, posting a notification) would see those stop happening. Enabling the cache is you asserting your `in:` is a pure fetch.
- **Requires `root:`.** The cache has to outlive the run that filled it; a cache under a temp directory the provider deletes would be a slower way to fetch once. Rejected at load time.
- **Keyed on content, not plan position** — a hash of the `in:` command, source, version, and execution settings (`image:`/`env:`/`user:`/`network:`), deliberately *not* the get node's merkle hash, so two jobs fetching the same version share one entry.
- **A hit costs a snapshot** (free on btrfs; a copy under `copy` — but still no fetch). **A failed fetch is never cached.** **A cache failure never fails a build** — anything wrong falls back to fetching. The startup sweep spares it.

## Step output cache (`volatile:`)

The resource cache above keeps what a `get` fetched. This one keeps what a **task or agent step produced**, so the steps that cost money can skip too.

It turns on by itself as soon as `workspace.root:` names a durable directory — there is no second switch. A step is looked up before it runs, and on a hit its declared `outputs:` are restored into the artifact store and the plan carries on with the next step:

```
skip: reviewer (reused)
```

Opt a single step out with `volatile:`:

```yaml
jobs:
- name: publish
  plan:
  - task: stamp
    volatile: true          # reads the clock; a recorded answer is a stale one
    outputs: [stamp]
    run: date +%s > stamp/at
    assert:
      files: [stamp/at]
  assert:
    execution: [stamp]
    outcome: succeeded
```

- **The key is the work, plus the bytes it reads.** A step's own hashed content (its command or prompt, image, tool grant, declared inputs/outputs and mappings) combined with a content digest of each input artifact as materialized. Deliberately **not** the step's merkle node hash, which carries the whole chain that led to it: two jobs doing the same work over the same bytes share one entry, and an upstream step that re-ran and answered *differently* changes the digest, so this step misses and runs. The digest covers paths, file bytes, the executable bit and symlink targets — never mtimes or ownership, which would miss on every run.
- **Agent steps are cached like task steps, and for the same reason.** An agent step is a task running a model; the contract in both directions is that a step's declared inputs identify its work. An agent reaching past them through `run_shell` is the same bargain as a task's `run:` doing it — which is what `volatile:` is for.
- **What is never cached**, because a hit restores declared outputs and nothing else: a step with `verdicts:` or `to:` (a decision the store records and routing keys on), one declaring `context: { from: ... }` (it reads an upstream decision the key cannot see), one with hooks (a hit fires none, like any skip), a task with `fix:`, and an `across:` cell. A `when:` guard is *not* a bar: it is evaluated first, and a step it lets through is work like any other.
- **`volatile:` is only valid on task and agent steps**, and never on a hook — anywhere else it would read as configured while binding nothing, so it is a load error.
- **Requires `root:`.** Same reason the resource cache does: an entry has to outlive the run that wrote it. Without one, nothing is cached and nothing is stored.
- **A hit costs a snapshot** (free on btrfs, a copy under `copy`), never a model call. **A failed step is never cached**, and **a cache failure never fails a build** — anything wrong falls back to running the step. Entries are evicted least-recently-used.

## Resuming a failed run

If a step fails, re-running the job from the beginning pays for every expensive step again — and for agent steps it is **lossy**: an agent is not deterministic, so re-running does not reproduce the output that already passed review.

```
$ steps run pipeline.yml --job publish
... 50 minutes ...
steps: error: put "repo": failed to push some refs
run: K7QP2XM4  (resume with: steps run <pipeline> --resume K7QP2XM4)
run: K7QP2XM4  workspace kept at /tmp/steps-1837462

$ steps run pipeline.yml --resume K7QP2XM4
skip: planner       (already succeeded)
skip: coder         (already succeeded)
skip: reviewer      (already succeeded)
put: repo
```

- **This is not the merkle cache.** The cache asks "has this *content* succeeded before", which is deliberately never true for a put or an agent. A resume asks something narrower and answerable for exactly those steps: "did **this run** already do this one".
- **A failed run keeps its workspace**, and says where — the files a step had just written when it failed are what the resumed steps continue from.
- **The job name comes from the run id.** `--resume <id>` alone is enough.
- **Steps are recorded as done on success only.** A failed step is exactly the one a resume must run again.

See also [infra.md](infra.md) for `image:`, which composes with workspace isolation — a containerized step still only sees its declared inputs/outputs.
