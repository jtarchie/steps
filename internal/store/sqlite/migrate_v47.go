package sqlite

// rebuildV47 is migration 47's text: jobs and resources, seeded from every name already recorded so the upgrade deletes nothing, and each table that names one rebuilt with its reference. sqlite cannot add a foreign key to a table in place, so each is copied in rowid order (retention reads rowid as insertion order), dropped and renamed — which is only safe with enforcement off, or the DROP would cascade into every row that references it; upgrade turns it off and checks the result. FROZEN: this is what ran on a real database.
const rebuildV47 = `
CREATE TABLE jobs (
    pipeline_id INTEGER NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    retired     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (pipeline_id, name)
);
CREATE TABLE resources (
    pipeline_id INTEGER NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    PRIMARY KEY (pipeline_id, name)
);
INSERT INTO jobs (pipeline_id, name)
    SELECT pipeline_id, job_name FROM nodes
    UNION SELECT pipeline_id, job_name FROM job_runs
    UNION SELECT pipeline_id, job_name FROM trigger_queue
    UNION SELECT pipeline_id, job_name FROM job_versions
    UNION SELECT pipeline_id, job_name FROM job_version_cursor
    UNION SELECT pipeline_id, job_name FROM runs
    UNION SELECT pipeline_id, job_name FROM job_concurrency
    UNION SELECT pipeline_id, job_name FROM job_serial_groups
    UNION SELECT pipeline_id, job_name FROM job_breaker;
INSERT INTO resources (pipeline_id, name)
    SELECT pipeline_id, resource_name FROM resource_configs
    UNION SELECT pipeline_id, resource_name FROM resource_checks
    UNION SELECT pipeline_id, resource_name FROM resource_check_errors
    UNION SELECT pipeline_id, resource_name FROM resource_versions
    UNION SELECT pipeline_id, resource_name FROM job_version_cursor;
CREATE TABLE resource_configs_v47 (
    pipeline_id   INTEGER NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    resource_name TEXT NOT NULL,
    config_hash   TEXT NOT NULL,
    PRIMARY KEY (pipeline_id, resource_name),
    FOREIGN KEY (pipeline_id, resource_name) REFERENCES resources(pipeline_id, name) ON UPDATE CASCADE ON DELETE CASCADE
);
INSERT INTO resource_configs_v47 (pipeline_id, resource_name, config_hash)
    SELECT pipeline_id, resource_name, config_hash FROM resource_configs ORDER BY rowid;
DROP TABLE resource_configs;
ALTER TABLE resource_configs_v47 RENAME TO resource_configs;
CREATE TABLE resource_checks_v47 (
    pipeline_id   INTEGER NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    resource_name TEXT NOT NULL,
    version_json  TEXT NOT NULL,
    checked_at    TEXT NOT NULL,
    PRIMARY KEY (pipeline_id, resource_name),
    FOREIGN KEY (pipeline_id, resource_name) REFERENCES resources(pipeline_id, name) ON UPDATE CASCADE ON DELETE CASCADE
);
INSERT INTO resource_checks_v47 (pipeline_id, resource_name, version_json, checked_at)
    SELECT pipeline_id, resource_name, version_json, checked_at FROM resource_checks ORDER BY rowid;
DROP TABLE resource_checks;
ALTER TABLE resource_checks_v47 RENAME TO resource_checks;
CREATE TABLE resource_check_errors_v47 (
    pipeline_id   INTEGER NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    resource_name TEXT NOT NULL,
    message       TEXT NOT NULL,
    failed_at     TEXT NOT NULL,
    PRIMARY KEY (pipeline_id, resource_name),
    FOREIGN KEY (pipeline_id, resource_name) REFERENCES resources(pipeline_id, name) ON UPDATE CASCADE ON DELETE CASCADE
);
INSERT INTO resource_check_errors_v47 (pipeline_id, resource_name, message, failed_at)
    SELECT pipeline_id, resource_name, message, failed_at FROM resource_check_errors ORDER BY rowid;
DROP TABLE resource_check_errors;
ALTER TABLE resource_check_errors_v47 RENAME TO resource_check_errors;
CREATE TABLE resource_versions_v47 (
    pipeline_id   INTEGER NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    resource_name TEXT NOT NULL,
    version_json  TEXT NOT NULL,
    check_order   INTEGER NOT NULL,
    from_check    INTEGER NOT NULL DEFAULT 0,
    metadata      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (pipeline_id, resource_name, version_json),
    FOREIGN KEY (pipeline_id, resource_name) REFERENCES resources(pipeline_id, name) ON UPDATE CASCADE ON DELETE CASCADE
);
INSERT INTO resource_versions_v47 (pipeline_id, resource_name, version_json, check_order, from_check, metadata)
    SELECT pipeline_id, resource_name, version_json, check_order, from_check, metadata FROM resource_versions ORDER BY rowid;
DROP TABLE resource_versions;
ALTER TABLE resource_versions_v47 RENAME TO resource_versions;
CREATE INDEX idx_resource_versions_order
    ON resource_versions(pipeline_id, resource_name, check_order);
CREATE TABLE job_versions_v47 (
    pipeline_id   INTEGER NOT NULL,
    job_name      TEXT NOT NULL,
    resource_name TEXT NOT NULL,
    version_json  TEXT NOT NULL,
    recorded_at   TEXT NOT NULL,
    build_id      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (pipeline_id, job_name, resource_name, version_json),
    FOREIGN KEY (pipeline_id, resource_name, version_json)
        REFERENCES resource_versions(pipeline_id, resource_name, version_json) ON DELETE CASCADE,
    FOREIGN KEY (pipeline_id, job_name) REFERENCES jobs(pipeline_id, name) ON UPDATE CASCADE ON DELETE CASCADE
);
INSERT INTO job_versions_v47 (pipeline_id, job_name, resource_name, version_json, recorded_at, build_id)
    SELECT pipeline_id, job_name, resource_name, version_json, recorded_at, build_id FROM job_versions ORDER BY rowid;
DROP TABLE job_versions;
ALTER TABLE job_versions_v47 RENAME TO job_versions;
CREATE INDEX idx_job_versions_version ON job_versions(pipeline_id, resource_name, version_json);
CREATE TABLE job_version_cursor_v47 (
    pipeline_id   INTEGER NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    job_name      TEXT NOT NULL,
    resource_name TEXT NOT NULL,
    check_order   INTEGER NOT NULL,
    PRIMARY KEY (pipeline_id, job_name, resource_name),
    FOREIGN KEY (pipeline_id, job_name) REFERENCES jobs(pipeline_id, name) ON UPDATE CASCADE ON DELETE CASCADE,
    FOREIGN KEY (pipeline_id, resource_name) REFERENCES resources(pipeline_id, name) ON UPDATE CASCADE ON DELETE CASCADE
);
INSERT INTO job_version_cursor_v47 (pipeline_id, job_name, resource_name, check_order)
    SELECT pipeline_id, job_name, resource_name, check_order FROM job_version_cursor ORDER BY rowid;
DROP TABLE job_version_cursor;
ALTER TABLE job_version_cursor_v47 RENAME TO job_version_cursor;
CREATE INDEX idx_job_version_cursor_resource ON job_version_cursor(pipeline_id, resource_name);
CREATE TABLE nodes_v47 (
    pipeline_id INTEGER NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    hash        TEXT NOT NULL,
    parent_hash TEXT,
    kind        TEXT NOT NULL,
    job_name    TEXT NOT NULL,
    resource    TEXT NOT NULL,
    step_index  INTEGER NOT NULL,
    content_hash TEXT NOT NULL REFERENCES node_content(content_hash) ON DELETE RESTRICT,
    result      TEXT,
    status      TEXT NOT NULL,
    error       TEXT,
    created_at  TEXT NOT NULL,
    PRIMARY KEY (pipeline_id, hash),
    FOREIGN KEY (pipeline_id, job_name) REFERENCES jobs(pipeline_id, name) ON UPDATE CASCADE ON DELETE CASCADE
);
INSERT INTO nodes_v47 (pipeline_id, hash, parent_hash, kind, job_name, resource, step_index, content_hash, result, status, error, created_at)
    SELECT pipeline_id, hash, parent_hash, kind, job_name, resource, step_index, content_hash, result, status, error, created_at FROM nodes ORDER BY rowid;
DROP TABLE nodes;
ALTER TABLE nodes_v47 RENAME TO nodes;
CREATE INDEX idx_nodes_job ON nodes(pipeline_id, job_name);
CREATE INDEX idx_nodes_content_hash ON nodes(content_hash);
CREATE TABLE job_runs_v47 (
    pipeline_id INTEGER NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    job_name   TEXT NOT NULL,
    root_hash  TEXT NOT NULL,
    PRIMARY KEY (pipeline_id, job_name, root_hash),
    FOREIGN KEY (pipeline_id, job_name) REFERENCES jobs(pipeline_id, name) ON UPDATE CASCADE ON DELETE CASCADE
);
INSERT INTO job_runs_v47 (pipeline_id, job_name, root_hash)
    SELECT pipeline_id, job_name, root_hash FROM job_runs ORDER BY rowid;
DROP TABLE job_runs;
ALTER TABLE job_runs_v47 RENAME TO job_runs;
CREATE TABLE runs_v47 (
    id         TEXT PRIMARY KEY,
    pipeline_id INTEGER NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    job_name   TEXT NOT NULL,
    workspace  TEXT NOT NULL,
    status     TEXT NOT NULL,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    parent_run_id TEXT REFERENCES runs(id) ON DELETE SET NULL,
    rerun_of       TEXT REFERENCES runs(id) ON DELETE SET NULL,
    revision_id INTEGER REFERENCES pipeline_revisions(id) ON DELETE RESTRICT,
    created_by  TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (pipeline_id, job_name) REFERENCES jobs(pipeline_id, name) ON UPDATE CASCADE ON DELETE CASCADE
);
INSERT INTO runs_v47 (id, pipeline_id, job_name, workspace, status, started_at, finished_at, parent_run_id, rerun_of, revision_id, created_by)
    SELECT id, pipeline_id, job_name, workspace, status, started_at, finished_at, parent_run_id, rerun_of, revision_id, created_by FROM runs ORDER BY rowid;
DROP TABLE runs;
ALTER TABLE runs_v47 RENAME TO runs;
CREATE INDEX idx_runs_job_started ON runs(pipeline_id, job_name, started_at);
CREATE INDEX idx_runs_started ON runs(pipeline_id, started_at);
CREATE INDEX idx_runs_parent ON runs(parent_run_id);
CREATE INDEX idx_runs_rerun_of ON runs(rerun_of);
CREATE INDEX idx_runs_revision ON runs(revision_id);
CREATE TABLE trigger_queue_v47 (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    pipeline_id INTEGER NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    job_name    TEXT NOT NULL,
    reason      TEXT NOT NULL,
    manual      INTEGER NOT NULL DEFAULT 0,
    rerun_of    TEXT REFERENCES runs(id) ON DELETE CASCADE,
    run_id      TEXT REFERENCES runs(id) ON DELETE SET NULL,
    requested_by TEXT NOT NULL DEFAULT '',
    pin         TEXT,
    status      TEXT NOT NULL,
    enqueued_at TEXT NOT NULL,
    started_at  TEXT,
    finished_at TEXT,
    error       TEXT,
    FOREIGN KEY (pipeline_id, job_name) REFERENCES jobs(pipeline_id, name) ON UPDATE CASCADE ON DELETE CASCADE
);
INSERT INTO trigger_queue_v47 (id, pipeline_id, job_name, reason, manual, rerun_of, run_id, requested_by, pin, status, enqueued_at, started_at, finished_at, error)
    SELECT id, pipeline_id, job_name, reason, manual, rerun_of, run_id, requested_by, pin, status, enqueued_at, started_at, finished_at, error FROM trigger_queue ORDER BY rowid;
DELETE FROM sqlite_sequence WHERE name = 'trigger_queue_v47';
INSERT INTO sqlite_sequence (name, seq) SELECT 'trigger_queue_v47', seq FROM sqlite_sequence WHERE name = 'trigger_queue';
DROP TABLE trigger_queue;
ALTER TABLE trigger_queue_v47 RENAME TO trigger_queue;
CREATE UNIQUE INDEX idx_trigger_queue_pending_job
    ON trigger_queue(pipeline_id, job_name) WHERE status = 'pending' AND rerun_of IS NULL AND run_id IS NULL AND pin IS NULL;
CREATE UNIQUE INDEX idx_trigger_queue_pending_pin
    ON trigger_queue(pipeline_id, job_name, pin) WHERE status = 'pending' AND pin IS NOT NULL AND run_id IS NULL;
CREATE UNIQUE INDEX idx_trigger_queue_pending_rerun
    ON trigger_queue(pipeline_id, rerun_of) WHERE status = 'pending' AND rerun_of IS NOT NULL AND run_id IS NULL;
CREATE INDEX idx_trigger_queue_rerun_of ON trigger_queue(rerun_of);
CREATE INDEX idx_trigger_queue_run ON trigger_queue(run_id);
CREATE INDEX idx_trigger_queue_job_status ON trigger_queue(pipeline_id, job_name, status);
CREATE TABLE job_concurrency_v47 (
    pipeline_id   INTEGER NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    job_name      TEXT NOT NULL,
    max_in_flight INTEGER NOT NULL,
    PRIMARY KEY (pipeline_id, job_name),
    FOREIGN KEY (pipeline_id, job_name) REFERENCES jobs(pipeline_id, name) ON UPDATE CASCADE ON DELETE CASCADE
);
INSERT INTO job_concurrency_v47 (pipeline_id, job_name, max_in_flight)
    SELECT pipeline_id, job_name, max_in_flight FROM job_concurrency ORDER BY rowid;
DROP TABLE job_concurrency;
ALTER TABLE job_concurrency_v47 RENAME TO job_concurrency;
CREATE TABLE job_serial_groups_v47 (
    pipeline_id INTEGER NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    job_name   TEXT NOT NULL,
    group_name TEXT NOT NULL,
    PRIMARY KEY (pipeline_id, job_name, group_name),
    FOREIGN KEY (pipeline_id, job_name) REFERENCES jobs(pipeline_id, name) ON UPDATE CASCADE ON DELETE CASCADE
);
INSERT INTO job_serial_groups_v47 (pipeline_id, job_name, group_name)
    SELECT pipeline_id, job_name, group_name FROM job_serial_groups ORDER BY rowid;
DROP TABLE job_serial_groups;
ALTER TABLE job_serial_groups_v47 RENAME TO job_serial_groups;
CREATE TABLE job_breaker_v47 (
    pipeline_id INTEGER NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    job_name    TEXT NOT NULL,
    consecutive INTEGER NOT NULL,
    paused_at   TEXT,
    PRIMARY KEY (pipeline_id, job_name),
    FOREIGN KEY (pipeline_id, job_name) REFERENCES jobs(pipeline_id, name) ON UPDATE CASCADE ON DELETE CASCADE
);
INSERT INTO job_breaker_v47 (pipeline_id, job_name, consecutive, paused_at)
    SELECT pipeline_id, job_name, consecutive, paused_at FROM job_breaker ORDER BY rowid;
DROP TABLE job_breaker;
ALTER TABLE job_breaker_v47 RENAME TO job_breaker;
` + ensureParents
