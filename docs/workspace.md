# Workspace Isolation

By default every step in a triggered build shares one mutable directory: a `get` fetches into `<workspace>/<resource-name>/`, and every `task`/`agent`/`put` step after it runs with that same directory as its cwd — so one task can silently corrupt state for a later step. A top-level `workspace:` block opts a pipeline into Concourse-style per-step isolation instead. See `examples/workspace.yml`.

```yaml
workspace:
  strategy: copy   # or: btrfs (Linux only)
  root: /path       # optional for copy; required for btrfs
  options:
    compression: zstd   # btrfs only: zstd | lzo | zlib | none
```

- **`inputs:`/`outputs:`** on a `task`/`agent`/`put` step (and on a top-level `tasks:` entry, overridable per step the same way `fix:` is) name artifacts — a resource an earlier `get` fetched, or an output an earlier `task`/`agent` produced. Under a `workspace:` block a step sees *only* what it declares: an isolated task/agent's working directory contains an `<input>/` copy (or, on btrfs, an instant copy-on-write snapshot) of each named input plus an empty `<output>/` dir per declared output, captured back into the build's artifact store after the step succeeds. `put` steps compose a read view the same way from their own `inputs:` — there's no implicit "all artifacts so far" view (but see `inputs: all` below).
- **`inputs:`/`outputs:` are optional and default to empty.** An absent `inputs:` means the step declares nothing; there is no requirement to write `inputs: []`. Declaring them buys two things: under a `workspace:` block they scope what the step physically sees, and everywhere they are flow-validated (see next bullet). An agent step's `dir:` also names the artifact it works in (its first path component) and is validated the same way.
- **When you declare inputs, they are validated against producers — `workspace:` block or not.** `ValidateArtifactFlow` runs for every job (even under `--force`): an `inputs:` (or agent `dir:`) naming an artifact nothing earlier fetched/produced is an error whether or not isolation is on. This turns "this step reads an artifact nobody produced" — e.g. an agent told to summarize a repo that was never fetched — into a plan-time failure, before any command or model runs. Without a `workspace:` block every step still shares the one mutable directory and physically sees everything; the declarations are a validated contract, not isolation.
- **Name mapping (`input_mapping:`/`output_mapping:`, task steps under `workspace:` only).** `{task-config-name: plan-artifact-name}`, mirroring Concourse: a reusable `tasks:` entry with pinned input/output names can be pointed at whatever a job actually fetched/produced without editing the task. The directory on disk keeps the task-config name; the artifact copied in / captured out uses the plan name. Mapping keys must be a subset of the resolved task's declared inputs/outputs. It's a load-time error on a non-task step or without a `workspace:` block (mapping renames a materialized directory, which the shared single directory can't do).
- **`put inputs: all`** composes the read view from every artifact produced so far — the escape hatch versus naming each explicitly. `all` is valid only on `put` steps. (Concourse's `detect` default is deliberately not adopted — too magic with templated params.)
- This is corruption hygiene, not a sandbox: shell commands can still reach outside the materialized directory via absolute paths, same as always.
- A `Provider` is built once per CLI invocation and validated at startup (wrong platform, wrong filesystem, missing binaries all fail fast, before any step runs).
- **Leftovers from a crashed run are swept at startup.** A build directory is normally removed when the build closes, so one still present under `root:` belongs to a process that never got there — a SIGKILL, a panicking host, a pulled plug. Under `strategy: btrfs` those directories hold live subvolumes that ordinary removal doesn't reclaim, so without this they accumulate and the disk is only recoverable with `btrfs subvolume delete` by hand. The sweep only touches directories this provider creates (`b-…`), is best-effort (a failure is logged, never fatal), and is **skipped entirely under `--keep-workspace`** — that flag means "leave the files for me to look at", so deleting the previous run's workspace at the start of the next one would defeat it.
- **Build directory names carry a per-invocation token.** The per-build counter restarts at 1 in every process, so two runs sharing a persistent `root:` would otherwise both want `b-1-<job>`: the directory create succeeds on the existing one and the backend then fails creating an artifact tree that's already there. The token makes that impossible even when the sweep is skipped or two processes share a root. The default (no-`workspace:`) implementation makes every method a no-op passthrough to the single shared directory, so pipelines that don't opt in pay nothing.
- Fix agents (`fix:`) run inside the failing task's own already-materialized working directory, not a fresh one — they need to see the exact state the task failed in, and the enclosing task's output capture (after the fix loop's final green verdict) is what actually persists outputs downstream.
- **Caching hashes fold in `inputs:`/`outputs:`/mappings/`inputs: all` only when a pipeline has a `workspace:` block** — so switching a pipeline into isolated mode invalidates its cache (correctly: the executed step's view changed), but a pipeline that never opts in hashes exactly as it always has (adding the now-required `inputs:` to a shared-mode pipeline does *not* invalidate its cache). The `strategy`/`root`/`options` themselves are never hashed — copy and btrfs produce the same logical view, so switching backends doesn't invalidate anyone's cache.

See also [infra.md](infra.md) for `image:`, which composes with workspace isolation (a containerized step still only sees its declared inputs/outputs), and for get renaming (`resource:`).

## Cross-build resource cache (`cache:`)

Within one build, `strategy: btrfs` already makes materialization free — a `get` lands in an artifact subvolume and every step snapshots from it, copying no bytes. Across builds there was nothing. Agent and `put` steps make their chains unskippable (see [the caching section above](#workspace-isolation)), so a real run **re-fetches every `get` from scratch**, paying the network and the disk again every time. That is the gap between what this does and what baggageclaim does for Concourse, where a fetched version is a shared copy-on-write parent.

```yaml
workspace:
  strategy: btrfs
  root: /mnt/btrfs
  cache:
    resources: true
    max_entries: 50    # optional; least-recently-used evicted first
```

- **Off by default, and the reason matters.** A cached version's `in:` does **not** run again. That is correct under the resource contract — `in:` materializes a version, and the same version materializes the same content (see [conformance.md](conformance.md)) — but a resource type whose `in:` has side effects beyond writing the directory (bumping a counter, posting a notification) would see those stop happening. Enabling the cache is you asserting your `in:` is a pure fetch.
- **Requires `root:`.** The cache has to outlive the run that filled it; without an explicit root the provider materializes under a temp directory it removes at the end, so a cache there would be a slower way to fetch once. Rejected at load time.
- **Keyed on content, not on plan position.** The key is a hash of the `in:` command, the source, the version, and the execution settings (`image:`/`env:`/`user:`/`network:`) — deliberately *not* the get node's merkle hash, which carries the artifact name and chains onto a parent, so two jobs fetching the same version would each get their own copy of identical bytes.
- **A hit costs a snapshot.** On btrfs that is instant and copies nothing; on `copy` it is a copy, but still no fetch.
- **A failed fetch is never cached** — the entry is seeded only after `in:` succeeds, so a half-written tree can't poison later builds.
- **Bounded.** `max_entries` (default 50) evicts least-recently-used, tracked by mtime and refreshed on every hit. An unbounded cache on a long-lived `steps watch` host fills the disk eventually.
- **The startup sweep spares it.** Leftover build directories are swept (above); the cache sits under the same root and is the one thing there worth keeping.
- A cache failure never fails a build: anything that goes wrong reading, writing, or pruning falls back to fetching.

## Resuming a failed run

If a step fails, the whole job had to be re-run from the beginning — every expensive step that already succeeded, paid for again. For a plan of shell commands that is barely noticeable. For one with agent steps it is the difference between a 2-minute recovery and a 50-minute one, in real money, and it is **lossy**: an agent is not deterministic, so re-running does not reproduce the output that already passed review, it produces a different one.

```
$ steps run pipeline.yml --job publish
... 50 minutes ...
steps: error: put "repo": failed to push some refs
run: K7QP2XM4  (resume with: steps run <pipeline> --resume K7QP2XM4)
run: K7QP2XM4  workspace kept at /tmp/steps-1837462

$ steps run pipeline.yml --resume K7QP2XM4
skip: planner       (already succeeded)
skip: coder         (already succeeded)
skip: build-check   (already succeeded)
skip: reviewer      (already succeeded)
put: repo
```

- **This is not the merkle cache.** The cache asks "has this *content* succeeded before", which is deliberately never true for a put or an agent — side effects and non-determinism. A resume asks something narrower and answerable for exactly those steps: "did **this run** already do this one".
- **A failed run keeps its workspace**, and says where. The files a step had just written when it failed are the most useful thing to look at, and they are what the resumed steps continue from — without them, "resume" would mean running the remaining steps against empty inputs and calling it a recovery.
- **The job name comes from the run id.** `--resume <id>` alone is enough; being asked to remember which job an id belonged to would make the id useless on its own.
- **Only the default shared workspace can be resumed.** A `workspace:` strategy builds and tears down a directory per step, so there is no tree left to continue in. `--resume` refuses rather than pretending.
- **Steps are recorded as done on success only.** A failed step is exactly the one a resume must run again.
