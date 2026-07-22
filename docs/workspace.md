# Workspace Isolation

By default every step in a triggered build shares one mutable directory: a `get` fetches into `<workspace>/<resource-name>/`, and every `task`/`agent`/`put` step after it runs with that same directory as its cwd — so one task can silently corrupt state for a later step. A top-level `workspace:` block opts a pipeline into Concourse-style per-step isolation instead. See `examples/workspace.yml`.

```yaml
workspace:
  strategy: copy   # or: btrfs (Linux only)
  root: /path       # optional for copy; required for btrfs
  options:
    compression: zstd   # btrfs only: zstd | lzo | zlib | none
```

- **`inputs:`/`outputs:`** on a `task`/`agent`/`put` step (and on a top-level `tasks:` entry, overridable per step the same way `fix:` is) name artifacts — a resource an earlier `get` fetched, or an output an earlier `task`/`agent` produced. A step sees *only* what it declares: an isolated task/agent's working directory contains an `<input>/` copy (or, on btrfs, an instant copy-on-write snapshot) of each named input plus an empty `<output>/` dir per declared output, captured back into the build's artifact store after the step succeeds. `put` steps compose a read view the same way from their own `inputs:` — there's no implicit "all artifacts so far" view.
- This is corruption hygiene, not a sandbox: shell commands can still reach outside the materialized directory via absolute paths, same as always.
- A `Provider` is built once per CLI invocation and validated at startup (wrong platform, wrong filesystem, missing binaries all fail fast, before any step runs). The default (no-`workspace:`) implementation makes every method a no-op passthrough to the single shared directory, so pipelines that don't opt in pay nothing.
- Fix agents (`fix:`) run inside the failing task's own already-materialized working directory, not a fresh one — they need to see the exact state the task failed in, and the enclosing task's output capture (after the fix loop's final green verdict) is what actually persists outputs downstream.
- Declaring `inputs:`/`outputs:` without a `workspace:` block is a load-time error. An `inputs:` naming an artifact nothing earlier in the plan fetched/produced is a run-time error that runs unconditionally, even under `--force`.
- **Merkle hashes fold in `inputs:`/`outputs:` only when a pipeline has a `workspace:` block** — so switching a pipeline into isolated mode invalidates its cache (correctly: the executed step's inputs changed), but a pipeline that never opts in hashes exactly as it always has. The `strategy`/`root`/`options` themselves are never hashed — copy and btrfs produce the same logical view, so switching backends doesn't invalidate anyone's cache.

See also [infra.md](infra.md) for `image:`, which composes with workspace isolation (a containerized step still only sees its declared inputs/outputs).
