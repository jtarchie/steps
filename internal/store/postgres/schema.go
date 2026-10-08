package postgres

// schemaVersion is stamped into the schema_version table and checked on open.
//
// BUMP IT whenever the DDL below changes in a way an existing schema cannot
// satisfy — a new column, a new NOT NULL, a changed key. It is a detector, not
// a migration counter, with the sqlite driver's policy: a mismatch is refused,
// and the answer is DROP SCHEMA, which loses steps' data and nothing else.
//
// Its own counter, not the sqlite one: the two schemas are written separately
// and change separately.
const schemaVersion = 3

// schema is the sqlite driver's schema in Postgres's own terms; the reason each
// table and column exists is written there (internal/store/sqlite/schema.go)
// and not repeated. What differs, and why:
//
//   - Every table sqlite orders by rowid carries seq, an identity column, since
//     Postgres has no insertion order of its own. Retention's count caps and
//     every "newest first" tie-break read it.
//   - Timestamps are timestamptz. Ties within a microsecond are broken by seq
//     or id, which is what sortableNano's fixed width did for sqlite's TEXT.
//   - Every foreign key's child columns are indexed: Postgres, unlike sqlite,
//     does not need one to enforce a key, and without one every cascading
//     delete scans the child table.
//   - version_json stays TEXT rather than jsonb. It is a KEY, compared byte
//     for byte, and jsonb normalizes (and refuses \u0000).
//   - pipelines.current_revision_id is added after pipeline_revisions exists:
//     the two reference each other, and Postgres resolves a reference when it
//     is declared.
//
// The three columns that deliberately reference nothing — nodes.parent_hash,
// job_runs.root_hash and run_events.parent_step_id — stay that way, for the
// reasons sqlite's schema gives.
//
// Table names are unqualified: every connection's search_path is this
// driver's schema and nothing else (see connConfig).
const schema = `
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS pipelines (
    id   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    path TEXT NOT NULL,
    current_revision_id BIGINT,
    paused_at TIMESTAMPTZ,
    set_at    TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS pipeline_revisions (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pipeline_id BIGINT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    sha         TEXT NOT NULL,
    source      TEXT NOT NULL,
    loaded_at   TIMESTAMPTZ NOT NULL,
    UNIQUE (pipeline_id, sha)
);

ALTER TABLE pipelines ADD CONSTRAINT pipelines_current_revision_id_fkey
    FOREIGN KEY (current_revision_id) REFERENCES pipeline_revisions(id) ON DELETE RESTRICT;
CREATE INDEX IF NOT EXISTS idx_pipelines_revision ON pipelines(current_revision_id);

CREATE TABLE IF NOT EXISTS revision_includes (
    revision_id BIGINT NOT NULL REFERENCES pipeline_revisions(id) ON DELETE CASCADE,
    path        TEXT NOT NULL,
    -- BYTEA: the sha covers these bytes, and a script need not be UTF-8.
    content     BYTEA NOT NULL,
    PRIMARY KEY (revision_id, path)
);

CREATE TABLE IF NOT EXISTS node_content (
    content_hash TEXT PRIMARY KEY,
    content      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS nodes (
    seq         BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    pipeline_id BIGINT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    hash        TEXT NOT NULL,
    -- No foreign key: a container node is recorded after the children that
    -- hashed under it. See the sqlite schema.
    parent_hash TEXT,
    kind        TEXT NOT NULL,
    job_name    TEXT NOT NULL,
    resource    TEXT NOT NULL,
    step_index  INTEGER NOT NULL,
    content_hash TEXT NOT NULL REFERENCES node_content(content_hash) ON DELETE RESTRICT,
    result      TEXT,
    status      TEXT NOT NULL,
    error       TEXT,
    created_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (pipeline_id, hash)
);
CREATE INDEX IF NOT EXISTS idx_nodes_job ON nodes(pipeline_id, job_name, seq);
CREATE INDEX IF NOT EXISTS idx_nodes_content_hash ON nodes(content_hash);

-- root_hash references nothing: a chain may end in a container, which has no
-- node, and cascading the skip index off node retention would re-run work
-- that succeeded. See the sqlite schema.
CREATE TABLE IF NOT EXISTS job_runs (
    seq         BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    pipeline_id BIGINT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    job_name   TEXT NOT NULL,
    root_hash  TEXT NOT NULL,
    PRIMARY KEY (pipeline_id, job_name, root_hash)
);

CREATE TABLE IF NOT EXISTS step_blobs (
    seq         BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    pipeline_id BIGINT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    action_key  TEXT NOT NULL,
    output      TEXT NOT NULL,
    digest      TEXT NOT NULL,
    PRIMARY KEY (pipeline_id, action_key, output)
);

CREATE TABLE IF NOT EXISTS resource_checks (
    pipeline_id   BIGINT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    resource_name TEXT NOT NULL,
    version_json  TEXT NOT NULL,
    checked_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (pipeline_id, resource_name)
);

CREATE TABLE IF NOT EXISTS resource_check_errors (
    pipeline_id   BIGINT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    resource_name TEXT NOT NULL,
    message       TEXT NOT NULL,
    failed_at     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (pipeline_id, resource_name)
);

CREATE TABLE IF NOT EXISTS resource_versions (
    pipeline_id   BIGINT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    resource_name TEXT NOT NULL,
    version_json  TEXT NOT NULL,
    check_order   BIGINT NOT NULL,
    from_check    BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (pipeline_id, resource_name, version_json)
);
CREATE INDEX IF NOT EXISTS idx_resource_versions_order
    ON resource_versions(pipeline_id, resource_name, check_order);

CREATE TABLE IF NOT EXISTS job_versions (
    seq           BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    pipeline_id   BIGINT NOT NULL,
    job_name      TEXT NOT NULL,
    resource_name TEXT NOT NULL,
    version_json  TEXT NOT NULL,
    recorded_at   TIMESTAMPTZ NOT NULL,
    build_id      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (pipeline_id, job_name, resource_name, version_json),
    FOREIGN KEY (pipeline_id, resource_name, version_json)
        REFERENCES resource_versions(pipeline_id, resource_name, version_json) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_job_versions_version ON job_versions(pipeline_id, resource_name, version_json);

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    pipeline_id   BIGINT NOT NULL,
    resource_name TEXT NOT NULL,
    version_json  TEXT NOT NULL,
    body          BYTEA NOT NULL,
    headers_json  TEXT NOT NULL,
    PRIMARY KEY (pipeline_id, resource_name, version_json),
    FOREIGN KEY (pipeline_id, resource_name, version_json)
        REFERENCES resource_versions(pipeline_id, resource_name, version_json) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS job_version_cursor (
    pipeline_id   BIGINT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    job_name      TEXT NOT NULL,
    resource_name TEXT NOT NULL,
    check_order   BIGINT NOT NULL,
    PRIMARY KEY (pipeline_id, job_name, resource_name)
);

CREATE TABLE IF NOT EXISTS runs (
    seq        BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    id         TEXT PRIMARY KEY,
    pipeline_id BIGINT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    job_name   TEXT NOT NULL,
    workspace  TEXT NOT NULL,
    status     TEXT NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    parent_run_id TEXT REFERENCES runs(id) ON DELETE SET NULL,
    rerun_of       TEXT REFERENCES runs(id) ON DELETE SET NULL,
    revision_id BIGINT REFERENCES pipeline_revisions(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_runs_job_started ON runs(pipeline_id, job_name, started_at);
CREATE INDEX IF NOT EXISTS idx_runs_started ON runs(pipeline_id, started_at);
CREATE INDEX IF NOT EXISTS idx_runs_parent ON runs(parent_run_id);
CREATE INDEX IF NOT EXISTS idx_runs_rerun_of ON runs(rerun_of);
CREATE INDEX IF NOT EXISTS idx_runs_revision ON runs(revision_id);

CREATE TABLE IF NOT EXISTS trigger_queue (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pipeline_id BIGINT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    job_name    TEXT NOT NULL,
    reason      TEXT NOT NULL,
    manual      BOOLEAN NOT NULL DEFAULT FALSE,
    rerun_of    TEXT REFERENCES runs(id) ON DELETE CASCADE,
    status      TEXT NOT NULL,
    enqueued_at TIMESTAMPTZ NOT NULL,
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    error       TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_trigger_queue_pending_job
    ON trigger_queue(pipeline_id, job_name) WHERE status = 'pending' AND rerun_of IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_trigger_queue_pending_rerun
    ON trigger_queue(pipeline_id, rerun_of) WHERE status = 'pending' AND rerun_of IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_trigger_queue_rerun_of ON trigger_queue(rerun_of);
CREATE INDEX IF NOT EXISTS idx_trigger_queue_job_status ON trigger_queue(pipeline_id, job_name, status);

CREATE TABLE IF NOT EXISTS run_steps (
    seq        BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    build_id   TEXT NOT NULL,
    step_index INTEGER NOT NULL,
    step_name  TEXT NOT NULL,
    PRIMARY KEY (run_id, build_id, step_index)
);

CREATE TABLE IF NOT EXISTS approvals (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pipeline_id  BIGINT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    job_name     TEXT NOT NULL,
    message      TEXT NOT NULL,
    status       TEXT NOT NULL,
    requested_at TIMESTAMPTZ NOT NULL,
    decided_at   TIMESTAMPTZ,
    decided_by   TEXT,
    reason       TEXT
);
CREATE INDEX IF NOT EXISTS idx_approvals_pipeline ON approvals(pipeline_id);

CREATE TABLE IF NOT EXISTS memories (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pipeline_id BIGINT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    scope       TEXT NOT NULL,
    text        TEXT NOT NULL,
    run_id      TEXT REFERENCES runs(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_memories_text ON memories(pipeline_id, scope, text);
CREATE INDEX IF NOT EXISTS idx_memories_scope ON memories(pipeline_id, scope, id);
CREATE INDEX IF NOT EXISTS idx_memories_run ON memories(run_id);

CREATE TABLE IF NOT EXISTS questions (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id          TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    job_name        TEXT NOT NULL,
    agent_name      TEXT NOT NULL,
    question        TEXT NOT NULL,
    options         TEXT NOT NULL,
    options_required BOOLEAN NOT NULL DEFAULT FALSE,
    default_answer  TEXT,
    memo_key        TEXT NOT NULL,
    status          TEXT NOT NULL,
    asked_at        TIMESTAMPTZ NOT NULL,
    answered_at     TIMESTAMPTZ,
    answered_by     TEXT,
    answer          TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_questions_memo ON questions(run_id, memo_key);
CREATE INDEX IF NOT EXISTS idx_questions_pending ON questions(status, id);

CREATE TABLE IF NOT EXISTS job_concurrency (
    pipeline_id   BIGINT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    job_name      TEXT NOT NULL,
    max_in_flight INTEGER NOT NULL,
    PRIMARY KEY (pipeline_id, job_name)
);

CREATE TABLE IF NOT EXISTS job_serial_groups (
    pipeline_id BIGINT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    job_name   TEXT NOT NULL,
    group_name TEXT NOT NULL,
    PRIMARY KEY (pipeline_id, job_name, group_name)
);

CREATE TABLE IF NOT EXISTS job_breaker (
    seq         BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    pipeline_id BIGINT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    job_name    TEXT NOT NULL,
    consecutive INTEGER NOT NULL,
    paused_at   TIMESTAMPTZ,
    PRIMARY KEY (pipeline_id, job_name)
);

CREATE TABLE IF NOT EXISTS run_events (
    seq        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    type       TEXT NOT NULL,
    step_index INTEGER NOT NULL,
    step_name  TEXT NOT NULL,
    step_kind  TEXT NOT NULL,
    step_id        BIGINT NOT NULL DEFAULT 0,
    -- No foreign key: a container's events follow its children's. See the
    -- sqlite schema.
    parent_step_id BIGINT NOT NULL DEFAULT 0,
    status     TEXT NOT NULL,
    hash       TEXT NOT NULL,
    text       TEXT NOT NULL,
    name       TEXT NOT NULL,
    detail     TEXT NOT NULL,
    duration_ms BIGINT NOT NULL,
    worker     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_run_events_run ON run_events(run_id, seq);
CREATE INDEX IF NOT EXISTS idx_run_events_hash ON run_events(hash);

CREATE TABLE IF NOT EXISTS agent_usage (
    seq               BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    run_id            TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    pipeline_id       BIGINT NOT NULL,
    step_index        INTEGER NOT NULL,
    step_name         TEXT NOT NULL,
    job_name          TEXT NOT NULL,
    node_hash         TEXT NOT NULL,
    model_requested   TEXT NOT NULL,
    model_served      TEXT NOT NULL,
    prompt_tokens     BIGINT NOT NULL,
    completion_tokens BIGINT NOT NULL,
    total_tokens      BIGINT NOT NULL,
    cached_tokens     BIGINT NOT NULL,
    reasoning_tokens  BIGINT NOT NULL,
    cost_usd          DOUBLE PRECISION,
    finish_reason     TEXT NOT NULL,
    duration_ms       BIGINT NOT NULL,
    raw_meta          TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (pipeline_id, run_id, node_hash),
    FOREIGN KEY (pipeline_id, node_hash) REFERENCES nodes(pipeline_id, hash) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_agent_usage_run ON agent_usage(run_id, step_index);
CREATE INDEX IF NOT EXISTS idx_agent_usage_node ON agent_usage(pipeline_id, node_hash);

CREATE TABLE IF NOT EXISTS run_inputs (
    run_id        TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    input_name    TEXT NOT NULL,
    resource_name TEXT NOT NULL,
    version_json  TEXT NOT NULL,
    PRIMARY KEY (run_id, input_name)
);

-- node_hash is NULL for a hook, and MATCH SIMPLE exempts a row whose key has
-- a NULL from the composite reference, as sqlite does.
CREATE TABLE IF NOT EXISTS run_placements (
    run_id        TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    pipeline_id   BIGINT NOT NULL,
    step_index    INTEGER NOT NULL,
    step_name     TEXT NOT NULL,
    job_name      TEXT NOT NULL,
    slot          TEXT NOT NULL,
    node_hash     TEXT,
    tag           TEXT NOT NULL,
    address       TEXT NOT NULL,
    instance_id   TEXT,
    goos          TEXT NOT NULL,
    goarch        TEXT NOT NULL,
    workdir       TEXT NOT NULL,
    fstype        TEXT NOT NULL,
    fs_free       BIGINT NOT NULL,
    uid           INTEGER,
    gid           INTEGER,
    image         TEXT NOT NULL,
    bytes_sent    BIGINT NOT NULL,
    bytes_received BIGINT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (pipeline_id, run_id, slot),
    FOREIGN KEY (pipeline_id, node_hash) REFERENCES nodes(pipeline_id, hash) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_run_placements_run ON run_placements(run_id, step_index);
CREATE INDEX IF NOT EXISTS idx_run_placements_node ON run_placements(pipeline_id, node_hash);

CREATE TABLE IF NOT EXISTS node_transcripts (
    pipeline_id BIGINT NOT NULL,
    hash       TEXT NOT NULL,
    transcript TEXT NOT NULL,
    PRIMARY KEY (pipeline_id, hash),
    FOREIGN KEY (pipeline_id, hash) REFERENCES nodes(pipeline_id, hash) ON DELETE CASCADE
);
`
