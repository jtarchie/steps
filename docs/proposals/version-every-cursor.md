# Proposal: `version: every` needs a cursor, not a bigger cache

## The bug this comes from

A Slack bot pipeline (`get: mentions, trigger: true, version: every` → agent →
`put: reply`) re-answered *every* mention still visible to its check, every
time a new one arrived. Three mentions in the window meant three replies
posted per trigger, two of them repeats.

Nothing was broken in the check: it returns the mentions that exist, which is
what a check is for. The repetition is in what `version: every` does with that
list.

## Why the merkle cache doesn't already cover this

The natural assumption — and the first one made when this was found — is that
content-addressed skipping should have caught it. The version IS in the node's
hashed content, so a get/task chain re-run against a version it already ran
hashes identically and is skipped.

It doesn't apply here because `internal/pipeline/route.go`'s
`unskippableReason` names two kinds of step that are **never** cache-skipped:

| step | why it can't be skipped |
|---|---|
| `agent` | non-deterministic; the same inputs do not imply the same output |
| `put` | its worth is an effect on the outside world, not an artifact |

That is correct and should stay correct. The cache's job is to avoid
*recomputing a value*; it was never a mechanism for avoiding *repeating an
effect*. A put deliberately re-posts when asked to, because "asked to" is the
only signal it has.

So the deduplication has to happen one layer up, in **scheduling**: decide
which versions this job should fan out over at all, rather than deciding
afterwards whether each resulting node can be skipped.

## What Concourse actually does

`version: every` in Concourse walks a **per-job cursor over versions that job
has not built yet** (`atc/db/versions_db.go`'s `NextEveryVersion`), and the
cursor advances regardless of build status — a claim already recorded in
[conformance.md](../conformance.md) and pinned by
`TestConformanceGetVersionEveryContinuesPastFailure`.

`steps` instead returns whatever `check` just printed
(`internal/resource/resource.go`'s `ResolveVersions`, the `mode == "every"`
branch). Two versions of the same list on two consecutive polls are two
identical fan-outs.

A second, related divergence: Concourse hands `check` the latest version it
knows about, so a resource can return only what came after it
(concourse-ci.org/docs/resource-types/implementing/). `steps` renders a check
against `{source}` alone, so a check *cannot* narrow its own output even when
the upstream API supports it (`oldest=` on Slack, `since=` on GitHub). Worth
fixing eventually; not required by this proposal, and orthogonal to it.

## Design

**1. A consumed-version set, in its own table.**

```sql
CREATE TABLE job_version_cursor (
    job_name      TEXT NOT NULL,
    resource_name TEXT NOT NULL,
    version_json  TEXT NOT NULL,
    consumed_at   TEXT NOT NULL,
    PRIMARY KEY (job_name, resource_name, version_json)
);
```

Deliberately **not** `job_versions`, which looks like it would fit. That table
answers "did this job go GREEN on this version", which is what `passed:` reads
(`RecordPassedVersion`, `HasPassedVersionSet`). The cursor answers "did this
job already TAKE this version", and advances on failure too. Same shape, two
different questions — and this repo has already paid once for keeping an
available answer to the wrong question around (see `HasPassedVersion`'s
deliberate absence in `store.go`).

A set rather than a high-water mark: versions have no stable total order
across checks (a check returns a list, and a resource may backfill), so
"everything before X" is a claim the data doesn't support. Membership is.

**2. One seam, so plan and run cannot disagree.**

`resource.Cache` is already the single point both callers go through:

- `internal/merkle/merkle.go:1352` (plan-time hashing)
- `internal/pipeline/pipeline.go:1364` (run-time execution)
- constructed once per `RunJob` at `internal/pipeline/pipeline.go:238`

Give it an optional predicate at construction:

```go
cache := rsrc.NewCache(rsrc.WithConsumed(func(resource string, version map[string]any) bool {
    return consumed[resource][versionKey(version)]
}))
```

`internal/pipeline` builds the predicate from the store (it has one);
`internal/resource` gains no store dependency, so the depguard graph is
unchanged. Because both callers read through the same cache, the filtered list
is identical at plan time and run time — the invariant `Cache`'s doc comment
already promises.

**3. Consume on chain completion, pass or fail.**

Concourse parity, and the safer of the two failure modes: a crash mid-chain
leaves the version unconsumed, so it is answered on a later run (at-least-once)
rather than silently dropped. Consuming *before* running would make a crash
lose the work permanently, which for the motivating case means a question
nobody ever answers.

**4. An empty fan-out is a quiet success.**

When every version is consumed, the get resolves to zero versions: the job is
green, nothing runs, and it says `get: mentions (no new versions)`. Under
`steps watch` this is the common case — a re-enqueue whose work is already
done — so it must be unremarkable, not an error.

**5. Bypasses that already exist keep working.**

`--force` ("ignore persisted state and re-run every step") ignores the cursor
too; a CLI `--version` pin already beats `every` in `ResolveVersions`. Both are
the existing escape hatches, so neither is new surface.

**6. Scope: `version: every` only.** `latest` and pinned resolve to a single
version whose repetition is already governed by the merkle cache.

## The divergence that remains, and must be documented

`steps` keeps no version history: **the check's current output is the entire
universe of versions.** The cursor can therefore only *suppress* — it cannot
resurrect a version that has scrolled out of the check's window. A Concourse
job that was down for an hour still builds everything it missed, because the
ATC stored those versions; a `steps watch` that was down for an hour answers
only what its next check still returns.

That is a real behavioural difference and belongs beside the claim, not in a
commit message. The honest framing: `version: every` means "every version I can
still see and have not already taken", and the window is the resource's
business (`limit: 20` in the Slack case).

## Tests

- `internal/store`: insert/read/idempotent re-insert of a cursor row.
- Conformance: `TestConformanceGetVersionEveryConsumesEachVersionOnce`, and an
  assertion that the existing
  `TestConformanceGetVersionEveryContinuesPastFailure` fixture *also* consumes
  the failed version (cursor advances regardless of status).
- Root package e2e (fake LLM, the only place the whole stack runs): two
  triggers over an overlapping check output; the agent runs once per version,
  never twice.
- `internal/merkle`: plan-time and run-time see the same filtered set — the
  invariant that keeps hashes honest.

## Open question

The table grows one row per (job, resource, version) forever. Simplest bound is
a per-(job, resource) cap pruned at write (keep the most recent N, N ≈ 1000);
a TTL would be wrong, since an old-but-visible version must stay suppressed for
as long as the check can still return it.
