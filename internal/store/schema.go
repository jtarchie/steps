package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const schema = `
CREATE TABLE IF NOT EXISTS nodes (
    hash        TEXT PRIMARY KEY,
    parent_hash TEXT NOT NULL,
    kind        TEXT NOT NULL,
    job_name    TEXT NOT NULL,
    resource    TEXT NOT NULL,
    step_index  INTEGER NOT NULL,
    content     TEXT NOT NULL,
    result      TEXT,
    status      TEXT NOT NULL,
    error       TEXT,
    created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_nodes_parent_hash ON nodes(parent_hash);

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
CREATE TABLE IF NOT EXISTS job_versions (
    job_name      TEXT NOT NULL,
    resource_name TEXT NOT NULL,
    version_json  TEXT NOT NULL,
    recorded_at   TEXT NOT NULL,
    build_id      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (job_name, resource_name, version_json)
);

-- Which versions a job has already FANNED OUT over under get: version: every.
--
-- Deliberately not job_versions above, which looks like it would fit: that
-- table answers "did this job go GREEN on this version" for passed:, while
-- this one answers "is this job DONE with this version" (see
-- internal/pipeline/cursor.go for why a failed build leaves it unconsumed).
-- Same shape, different question — and an available answer to the wrong
-- question is how the passed: bug came back once already.
--
-- A SET rather than a high-water mark, because versions have no stable total
-- order across checks: a check returns a list, and a resource may backfill.
-- "Everything before X" is a claim the data does not support; membership is.
CREATE TABLE IF NOT EXISTS job_version_cursor (
    job_name      TEXT NOT NULL,
    resource_name TEXT NOT NULL,
    version_json  TEXT NOT NULL,
    consumed_at   TEXT NOT NULL,
    PRIMARY KEY (job_name, resource_name, version_json)
);

-- One row per run invocation, with the steps it got through. It is what
-- --resume reads: not "has this content succeeded before" (that is the merkle
-- cache) but "did THIS run already do this step".
CREATE TABLE IF NOT EXISTS runs (
    id         TEXT PRIMARY KEY,
    job_name   TEXT NOT NULL,
    workspace  TEXT NOT NULL,
    status     TEXT NOT NULL,
    started_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS run_steps (
    run_id     TEXT NOT NULL,
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
CREATE TABLE IF NOT EXISTS run_events (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     TEXT NOT NULL,
    job_name   TEXT NOT NULL,
    type       TEXT NOT NULL,
    step_index INTEGER NOT NULL,
    step_name  TEXT NOT NULL,
    step_kind  TEXT NOT NULL,
    status     TEXT NOT NULL,
    hash       TEXT NOT NULL,
    text       TEXT NOT NULL,
    name       TEXT NOT NULL,
    detail     TEXT NOT NULL,
    duration_ms INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_run_events_run ON run_events(run_id, seq);

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
-- cost_usd is nullable and stays NULL today: nothing in the request path
-- reports a dollar figure yet. It is here rather than computed from a price
-- table because a bundled table goes stale every time any provider changes
-- rates, and a confidently wrong number is worse than an absent one — see
-- docs/agents.md. Reporting says "unpriced", never $0.00.
--
-- raw_meta keeps the provider's whole usage block. The schema has no
-- versioning beyond addedColumns, so a field not captured today cannot be
-- backfilled tomorrow.
CREATE TABLE IF NOT EXISTS agent_usage (
    run_id            TEXT NOT NULL,
    step_index        INTEGER NOT NULL,
    step_name         TEXT NOT NULL,
    job_name          TEXT NOT NULL,
    node_hash         TEXT NOT NULL,
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
-- nodes.result deliberately: result is loaded by planners and routed-to
-- successors on every run and must stay bounded, while a transcript is read
-- on demand ("what did this step actually say and do"). Same hash key as
-- nodes; replaces on re-record like nodes does.
CREATE TABLE IF NOT EXISTS node_transcripts (
    hash       TEXT PRIMARY KEY,
    job_name   TEXT NOT NULL,
    transcript TEXT NOT NULL,
    created_at TEXT NOT NULL
);
`

// addedColumns are columns added to tables that already shipped. The schema
// above is all CREATE TABLE IF NOT EXISTS, which silently does nothing to a
// database created by an earlier version — so a new column on an OLD table
// needs its own ALTER, or the field exists only for people who deleted their
// state.db.
var addedColumns = []struct{ table, column, decl string }{
	{"runs", "finished_at", "TEXT"},
	// Rows written before this column existed carry '', which no correlated
	// lookup can match (HasPassedVersionSet ignores them). That is the
	// conservative direction and the right one: passed: is a GATE, so the
	// upgrade holds a multi-resource fan-in back until its upstream job runs
	// once more and writes a correlated set, rather than waving through a
	// combination nobody can prove was ever green together. A single-resource
	// constraint — the common case — is unaffected, since one row is trivially
	// its own coherent set.
	{"job_versions", "build_id", "TEXT NOT NULL DEFAULT ''"},
	// The run a replay forked from. Empty for an ordinary run, which is every
	// run recorded before replay existed.
	{"runs", "parent_run_id", "TEXT NOT NULL DEFAULT ''"},
	// The versions the poll that enqueued this row resolved, as
	// {resource: [version, ...]}. Empty means none were supplied, which is
	// every row queued by hand (the web UI, a manual re-run) and every row
	// written before this column existed — those jobs resolve their own
	// versions by running check, exactly as they always did.
	{"trigger_queue", "versions_json", "TEXT NOT NULL DEFAULT ''"},
}

// addColumns applies addedColumns, treating "duplicate column name" as
// success. SQLite has no ADD COLUMN IF NOT EXISTS, and probing PRAGMA
// table_info first would be the same check with an extra round trip and a
// race between the check and the alter.
func addColumns(ctx context.Context, db *sql.DB) error {
	for _, add := range addedColumns {
		_, err := db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", add.table, add.column, add.decl))
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("could not add %s.%s: %w", add.table, add.column, err)
		}
	}

	return nil
}
