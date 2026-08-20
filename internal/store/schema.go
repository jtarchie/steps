package store

const schema = `
-- The interned merkle preimage: what each node's hash was computed FROM.
--
-- Its own table because the same content repeats across builds and used to be
-- stored once per node. A step's content is mostly the parts of a pipeline
-- that do not change between runs — an agent's prompt and tool definitions, a
-- task's script, a put's expr body — while the part that does change is the
-- version a get fetched, which is a few dozen bytes. Measured on the database
-- that prompted this: 12 nodes, 7 distinct contents, and content was the
-- largest column in the file.
--
-- Keyed by a hash OF THE CONTENT, not by the node hash. A node hash folds in
-- its parent (see merkle.HashNode), so two byte-identical steps at different
-- points in a chain hash differently as nodes and identically here — which is
-- exactly the sharing this table exists for.
--
-- Nothing cascades INTO it: a row is shared by an unknown number of nodes, so
-- one node's deletion says nothing about whether the content is still needed.
-- Retention sweeps the unreferenced rows explicitly (see PruneRuns), and the
-- RESTRICT on the referencing side is what makes a mistake there an error
-- rather than a dangling node.
CREATE TABLE IF NOT EXISTS node_content (
    content_hash TEXT PRIMARY KEY,
    content      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS nodes (
    hash        TEXT PRIMARY KEY,
    -- NULL for a chain's first step, which has no parent. It was '', which is
    -- the wrong shape for an absent reference — sqlite reads an empty string as
    -- a value that must exist and a NULL as nothing to check — and the reads
    -- already COALESCE it back to '' for their callers.
    --
    -- The one column here that deliberately declares NO foreign key, and the
    -- reason is structural rather than an oversight: a CONTAINER node
    -- (in_parallel, race, across, ensemble, do, try) is recorded once its
    -- branches finish, because its status is what they decided — while every
    -- node inside it already hashed under the container as its parent. The
    -- child therefore exists before the parent, legitimately and by design, so
    -- the reference could only ever be declared by recording placeholder rows
    -- for six kinds of block step. Nothing would be gained by it: no delete
    -- needs to cascade along this column (see pruneNodes, which nulls dangling
    -- links instead) and no query joins on it. It is the link a node-detail
    -- page renders, and nothing else.
    parent_hash TEXT,
    kind        TEXT NOT NULL,
    job_name    TEXT NOT NULL,
    resource    TEXT NOT NULL,
    step_index  INTEGER NOT NULL,
    content_hash TEXT NOT NULL REFERENCES node_content(content_hash) ON DELETE RESTRICT,
    result      TEXT,
    status      TEXT NOT NULL,
    error       TEXT,
    created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_nodes_parent_hash ON nodes(parent_hash);
-- Retention scans nodes by job (ordering by rowid, not created_at — see
-- pruneNodes) and sweeps node_content by what nodes still point at; both are
-- full scans without these.
CREATE INDEX IF NOT EXISTS idx_nodes_job ON nodes(job_name);
CREATE INDEX IF NOT EXISTS idx_nodes_content_hash ON nodes(content_hash);

-- root_hash names a node and deliberately does not reference one, for two
-- independent reasons.
--
-- It cannot: a chain's leaf may be a CONTAINER step, and a container records no
-- node of its own — do: and in_parallel: are documented that way, their
-- children recording themselves instead — so for a plan ending in a block there
-- is no row to point at. It surfaced as every such pipeline failing to record.
--
-- And it should not, even where the node does exist. This table is the
-- content-addressed SKIP index: a row means "this job has already run this
-- exact content". Cascading it away when a node row is reaped would make
-- retention silently re-run work that had succeeded, which is the opposite of
-- what a cache should do under pressure. The chain's identity is its hash, not
-- the survival of a row describing one of its steps.
--
-- Bounded by age instead (see pruneJobRuns), which is the bound that matches
-- what it holds: a chain last green before the retention window is one whose
-- content nobody is about to submit again.
CREATE TABLE IF NOT EXISTS job_runs (
    job_name   TEXT NOT NULL,
    root_hash  TEXT NOT NULL,
    status     TEXT NOT NULL,
    error      TEXT,
    created_at TEXT NOT NULL,
    PRIMARY KEY (job_name, root_hash)
);

CREATE TABLE IF NOT EXISTS resource_checks (
    resource_name TEXT PRIMARY KEY,
    version_json  TEXT NOT NULL,
    checked_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS trigger_queue (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    job_name    TEXT NOT NULL,
    reason      TEXT NOT NULL,
    status      TEXT NOT NULL,
    enqueued_at TEXT NOT NULL,
    started_at  TEXT,
    finished_at TEXT,
    error       TEXT
);
-- At most one pending row per job at a time; a running row isn't covered,
-- so a version change mid-run still enqueues a fresh pending row for after.
CREATE UNIQUE INDEX IF NOT EXISTS idx_trigger_queue_pending_job
    ON trigger_queue(job_name) WHERE status = 'pending';

-- Every version steps has ever seen of a resource. The thing whose absence
-- was, until now, the one STRUCTURAL divergence from Concourse: without it a
-- version that scrolled out of a check's window while nothing was watching
-- was simply gone, so a cursor could suppress a version but never resurrect
-- one, and a job that was down could not build what it missed.
--
-- check_order is the point of the table. Versions have no inherent order —
-- a check returns a list and an upstream may backfill — but order of
-- DISCOVERY is well defined, and that is what this records: assigned once,
-- when a version is first seen, and never revised. Concourse's
-- NextEveryVersion walks the same column for the same reason.
--
-- It is also what makes re-derivation safe again. A job used to be handed
-- the versions its poll resolved, because asking a cursor-driven check a
-- second time returns a different answer; with history it can simply look
-- them up, at plan time and run time alike.
--
-- from_check separates the two ways a row gets here, and the distinction is
-- load-bearing. A check filing what it saw (from_check = 1) is history: it is
-- complete for that resource, so a job can read it INSTEAD of checking. A row
-- implied by something else referencing it (from_check = 0) is not: every
-- steps run resolves its own versions and records what it took, which
-- creates a parent for the foreign key but says nothing about what else
-- exists. Treating those as history would let one remembered version hide
-- every version a check would have reported.
CREATE TABLE IF NOT EXISTS resource_versions (
    resource_name TEXT NOT NULL,
    version_json  TEXT NOT NULL,
    check_order   INTEGER NOT NULL,
    from_check    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (resource_name, version_json)
);
CREATE INDEX IF NOT EXISTS idx_resource_versions_order
    ON resource_versions(resource_name, check_order);

-- Which resource versions each job has SUCCESSFULLY run against. It is what
-- passed: reads.
--
-- build_id is what makes the question the RIGHT one. Without it this table
-- answers "has this version been green in that job", per resource and
-- independently — which admits a downstream job running against a COMBINATION
-- of versions that each passed upstream but never passed TOGETHER. Concourse
-- resolves passed: across the whole plan at once for exactly this reason.
--
-- With it, the question becomes "is there one build of that job where all of
-- these versions were green at once", which is what a fan-in actually needs.
--
-- The foreign key is what keeps this honest against retention: a version
-- pruned from history takes its green record with it. That is the correct
-- direction — a version no longer in history cannot be built, so a gate that
-- would clear for it must stay shut — but it does couple the two, and a
-- history cap set below what a slow downstream job needs will hold that job
-- back. See resourceVersionCap.
CREATE TABLE IF NOT EXISTS job_versions (
    job_name      TEXT NOT NULL,
    resource_name TEXT NOT NULL,
    version_json  TEXT NOT NULL,
    recorded_at   TEXT NOT NULL,
    build_id      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (job_name, resource_name, version_json),
    FOREIGN KEY (resource_name, version_json)
        REFERENCES resource_versions(resource_name, version_json) ON DELETE CASCADE
);

-- How far a job has FANNED OUT under get: version: every -- the highest
-- check_order it has taken, per resource.
--
-- Deliberately not job_versions above, which looks like it would fit: that
-- table answers "did this job go GREEN on this version" for passed:, while
-- this one answers "is this job DONE with this version". Same subject,
-- different question, and an available answer to the wrong one is how the
-- passed: bug came back once already.
--
-- This was a SET of consumed versions until resource_versions gave versions
-- a total order. The reason was sound while it held: a check returns a list
-- and an upstream may backfill, so "everything before X" was a claim the data
-- could not support and only membership could be recorded. check_order is
-- that claim's missing foundation -- order of DISCOVERY, which is well
-- defined even when order of existence is not.
--
-- A mark is not merely smaller than a set, it is safer. A set has to be
-- capped or it grows forever, and a capped set forgets its oldest members
-- while the versions they name are still offered -- so they read as unbuilt
-- and run again, which is the repetition this table exists to prevent. A
-- mark has no members to forget. It is also what Concourse records:
-- NextEveryVersion takes the next version above the highest check_order the
-- job has built.
--
-- No foreign key, because there is no version here to point at. Pruning
-- history cannot corrupt a mark: it is a threshold, and a threshold naming
-- an order that no longer exists still separates what is above it from what
-- is below.
CREATE TABLE IF NOT EXISTS job_version_cursor (
    job_name      TEXT NOT NULL,
    resource_name TEXT NOT NULL,
    check_order   INTEGER NOT NULL,
    PRIMARY KEY (job_name, resource_name)
);

-- One row per run invocation, with the steps it got through. It is what
-- --resume reads: not "has this content succeeded before" (that is the merkle
-- cache) but "did THIS run already do this step".
CREATE TABLE IF NOT EXISTS runs (
    id         TEXT PRIMARY KEY,
    job_name   TEXT NOT NULL,
    -- Kept for the life of the row, though it looks like dead weight once a run
    -- ends. Clearing it was tried and reverted: --resume continues a FAILED run
    -- from the tree deliberately left on disk, and --replay forks a SUCCEEDED one
    -- kept with --keep-workspace, so both statuses are read after the run is
    -- over. See FinishRun.
    workspace  TEXT NOT NULL,
    status     TEXT NOT NULL,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    -- NULL for an ordinary run; the run a replay forked from otherwise. NULL
    -- rather than '' for the reason nodes.parent_hash is (see above), and
    -- SET NULL rather than CASCADE because a forked run is a run in its own
    -- right — retention reaping its parent must not reach forward and delete
    -- the child too.
    parent_run_id TEXT REFERENCES runs(id) ON DELETE SET NULL
);
-- Retention orders a job's runs by recency to find the ones past the cap.
CREATE INDEX IF NOT EXISTS idx_runs_job_started ON runs(job_name, started_at);

CREATE TABLE IF NOT EXISTS run_steps (
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_index INTEGER NOT NULL,
    step_name  TEXT NOT NULL,
    PRIMARY KEY (run_id, step_index)
);

-- Human decisions on approval: steps. The row IS the audit trail; it must not
-- depend on external chat history.
CREATE TABLE IF NOT EXISTS approvals (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    job_name     TEXT NOT NULL,
    message      TEXT NOT NULL,
    status       TEXT NOT NULL,
    requested_at TEXT NOT NULL,
    decided_at   TEXT,
    decided_by   TEXT,
    reason       TEXT
);

-- How many builds of each job may run at once. Synced from config at startup,
-- for the reason job_serial_groups is: Store.ClaimNextJob decides admission in
-- one atomic UPDATE, so every input it needs has to be readable from SQL.
--
-- serial:/serial_groups: have already been folded in by the time a row is
-- written (see config.Job.EffectiveMaxInFlight), so this column is the final
-- answer rather than one of several things to combine here.
CREATE TABLE IF NOT EXISTS job_concurrency (
    job_name      TEXT PRIMARY KEY,
    max_in_flight INTEGER NOT NULL
);

-- Which serial groups each job belongs to. Synced from config at startup; it
-- lives in the database so the claim can stay a single atomic statement
-- rather than a read-then-claim with a race in the middle.
CREATE TABLE IF NOT EXISTS job_serial_groups (
    job_name   TEXT NOT NULL,
    group_name TEXT NOT NULL,
    PRIMARY KEY (job_name, group_name)
);

-- The watch circuit breaker: how many times in a row a job has failed, and
-- whether that has taken it out of the rotation.
CREATE TABLE IF NOT EXISTS job_breaker (
    job_name    TEXT PRIMARY KEY,
    consecutive INTEGER NOT NULL,
    paused_at   TEXT
);

-- Everything a run did, in the order it did it: the persisted side of the
-- run-event bus (internal/events). It is what makes a finished run read back
-- exactly as it read live — a post-hoc view rebuilt from a different source
-- than the live one drifts, and the drift surfaces mid-incident.
--
-- Deliberately append-only and run-scoped, unlike content-addressed nodes:
-- two runs sharing a cached node still have their own separate stories about
-- reaching it.
-- step_name/step_kind stay denormalized. They look like copies of what
-- (run_id, step_index) already determines, and step_name is not: a fan-out
-- cell reports its PARENT's plan index and distinguishes itself by name, so
-- several rows share an index and only the name tells them apart. job_name
-- used to ride along too, on the argument that it spared a join — but nothing
-- ever read it back, so it was 14 bytes an event buying nothing (runs.job_name
-- is there for anyone who needs it).
CREATE TABLE IF NOT EXISTS run_events (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    type       TEXT NOT NULL,
    step_index INTEGER NOT NULL,
    step_name  TEXT NOT NULL,
    step_kind  TEXT NOT NULL,
    -- The display tree: which step this is, and which container it ran inside
    -- (0 at the top of a plan). See events.Event for why it is minted per run
    -- rather than read off the merkle chain.
    --
    -- parent_step_id declares NO foreign key, and the reason is the one
    -- nodes.parent_hash already has: a container publishes step_finished
    -- AFTER the children that ran inside it, so the child's row legitimately
    -- precedes the parent's. It is also self-referential within one table,
    -- where a cascade would delete a whole subtree on a retention pass that
    -- only meant to trim one row. Both rows die together with their run.
    step_id        INTEGER NOT NULL DEFAULT 0,
    parent_step_id INTEGER NOT NULL DEFAULT 0,
    status     TEXT NOT NULL,
    hash       TEXT NOT NULL,
    text       TEXT NOT NULL,
    name       TEXT NOT NULL,
    detail     TEXT NOT NULL,
    duration_ms INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_run_events_run ON run_events(run_id, seq);
-- Retention asks "which nodes does a surviving run still name" on every build,
-- and RunsUsingNode asks the same question from the node detail page. Both are
-- an anti-join on hash, which without this scans the whole table — the largest
-- run-scoped one — inside a write transaction holding the exclusive lock.
CREATE INDEX IF NOT EXISTS idx_run_events_hash ON run_events(hash);

-- What each agent step spent, and the provider metadata that explains it.
--
-- Keyed by (run_id, node_hash), NOT by (run_id, step_index): every agent step
-- inside a block reports the BLOCK's plan index, so six across: cells, an
-- ensemble's members and a do:'s children all share one index. Keying on it
-- kept the last one and silently overwrote the rest — which on a six-cell
-- review matrix meant reporting one reviewer and under-counting the run by the
-- whole fan-out.
--
-- A node hash distinguishes them because that is exactly what it is for: each
-- cell renders different content and hashes differently. Two byte-identical
-- agent invocations in one run do collapse onto one row, which is correct —
-- identical content under one parent IS one node.
--
-- step_index stays as a column for ordering, alongside rowid so steps sharing
-- an index still read back in the order they were recorded.
--
-- cost_usd is nullable because "nobody priced this" and "this was free" are
-- different answers. A CLI-backed agent meters itself and reports a figure on
-- exit; every HTTP path reports tokens and nothing else, and its rows stay
-- NULL. It is never computed from a price table, because a bundled table goes
-- stale every time any provider changes rates and a confidently wrong number
-- is worse than an absent one — see docs/agents.md. A NULL reports as
-- "unpriced", never $0.00.
--
-- raw_meta keeps the provider's whole usage block. The schema has no
-- versioning and no migration path, so a field not captured today cannot be
-- backfilled tomorrow.
CREATE TABLE IF NOT EXISTS agent_usage (
    run_id            TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_index        INTEGER NOT NULL,
    step_name         TEXT NOT NULL,
    job_name          TEXT NOT NULL,
    node_hash         TEXT NOT NULL REFERENCES nodes(hash) ON DELETE CASCADE,
    model_requested   TEXT NOT NULL,
    model_served      TEXT NOT NULL,
    prompt_tokens     INTEGER NOT NULL,
    completion_tokens INTEGER NOT NULL,
    total_tokens      INTEGER NOT NULL,
    cached_tokens     INTEGER NOT NULL,
    reasoning_tokens  INTEGER NOT NULL,
    cost_usd          REAL,
    finish_reason     TEXT NOT NULL,
    duration_ms       INTEGER NOT NULL,
    raw_meta          TEXT NOT NULL,
    created_at        TEXT NOT NULL,
    PRIMARY KEY (run_id, node_hash)
);
CREATE INDEX IF NOT EXISTS idx_agent_usage_run ON agent_usage(run_id, step_index);

-- Full agent conversation transcripts, one row per agent node, kept OUT of
-- nodes.result deliberately: result rides along on every node listing (the
-- CLI's node table, the web UI's node and run pages) and must stay bounded,
-- while a transcript is read on demand ("what did this step actually say and
-- do"). Same hash key as nodes; replaces on re-record like nodes does.
--
-- The largest single value the schema stores, and the one whose bound was
-- missing: internal/agent caps a single tool RESULT, but a conversation has
-- unboundedly many turns, so the row itself needs MaxTranscriptBytes.
CREATE TABLE IF NOT EXISTS node_transcripts (
    hash       TEXT PRIMARY KEY REFERENCES nodes(hash) ON DELETE CASCADE,
    transcript TEXT NOT NULL
);
`

// steps has no released version and no migration path, deliberately: the schema
// is created by CREATE TABLE IF NOT EXISTS and nothing rewrites an older one.
// A database from before a schema change is not upgraded, it is deleted — the
// only thing in it that cannot be rebuilt by running the pipeline again is run
// history, and pre-production that is not worth a migration framework whose
// every entry is a permanent cost.
//
// What this replaced: an addedColumns list of ALTER statements, a rebuiltTables
// list pairing each table with a predicate for detecting its own legacy shape,
// and the drop-and-recreate pass that ran them. Roughly 150 lines existing only
// to carry state nobody is keeping.
//
// If that changes, the thing to add back is a real migration story — a version
// counter and ordered steps — not another table-shaped special case.
