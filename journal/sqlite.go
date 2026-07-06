package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, CGO-free
)

// SQLiteStore is the default durable Store.
type SQLiteStore struct {
	db *sql.DB
	// writeMu serializes every in-process write (Append, CreateRun, UpdateRun,
	// MemoPut). SQLite allows only one writer anyway; serializing here also
	// closes the read-then-write window inside Append (SELECT MAX(seq) then
	// INSERT) that otherwise races a concurrent UpdateRun into
	// SQLITE_BUSY_SNAPSHOT once multiple runs execute at once. Reads stay
	// lock-free under WAL; cross-process writers still rely on busy_timeout.
	writeMu sync.Mutex
}

// OpenSQLite opens (and migrates) the journal database at path.
func OpenSQLite(path string) (*SQLiteStore, error) {
	// modernc.org/sqlite registers as "sqlite". Busy timeout keeps
	// concurrent CLI invocations (run + runs list) from erroring.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("opening sqlite %s: %w", path, err)
	}
	s := &SQLiteStore{db: db}
	err = s.migrate()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating schema: %w", err)
	}
	return s, nil
}

func (s *SQLiteStore) migrate() error {
	_, err := s.db.ExecContext(context.Background(), `
CREATE TABLE IF NOT EXISTS runs (
    id            TEXT PRIMARY KEY,
    machine       TEXT NOT NULL,
    hash          TEXT NOT NULL,
    source        BLOB NOT NULL,
    assets        TEXT NOT NULL DEFAULT '{}',
    status        TEXT NOT NULL,
    current_state TEXT NOT NULL DEFAULT '',
    parent_run_id TEXT NOT NULL DEFAULT '',
    created       INTEGER NOT NULL,
    updated       INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
    run_id TEXT NOT NULL REFERENCES runs(id),
    seq    INTEGER NOT NULL,
    type   TEXT NOT NULL,
    ts     INTEGER NOT NULL,
    data   TEXT NOT NULL,
    PRIMARY KEY (run_id, seq)
);
CREATE TABLE IF NOT EXISTS memo (
    key     TEXT PRIMARY KEY,
    output  TEXT NOT NULL,
    created INTEGER NOT NULL
);`)
	if err != nil {
		return fmt.Errorf("creating schema: %w", err)
	}
	// Additive column for pre-existing databases (CREATE IF NOT EXISTS above
	// only helps fresh ones). A duplicate-column error means it is already
	// applied — tolerate it so migrate stays idempotent.
	_, err = s.db.ExecContext(context.Background(),
		`ALTER TABLE runs ADD COLUMN parent_run_id TEXT NOT NULL DEFAULT ''`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("adding parent_run_id column: %w", err)
	}
	return nil
}

// MemoGet looks up a cached output by input hash.
func (s *SQLiteStore) MemoGet(ctx context.Context, key string) (map[string]any, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT output FROM memo WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("querying memo: %w", err)
	}
	var out map[string]any
	err = json.Unmarshal([]byte(raw), &out)
	if err != nil {
		return nil, false, fmt.Errorf("decoding memo output: %w", err)
	}
	return out, true, nil
}

// MemoPut caches an output by input hash.
func (s *SQLiteStore) MemoPut(ctx context.Context, key string, output map[string]any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	raw, err := json.Marshal(output)
	if err != nil {
		return fmt.Errorf("encoding memo output: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO memo (key, output, created) VALUES (?, ?, ?)`,
		key, string(raw), time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("writing memo: %w", err)
	}
	return nil
}

// CreateRun inserts a new run row.
func (s *SQLiteStore) CreateRun(ctx context.Context, run *Run) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now().UnixMilli()
	assets, err := json.Marshal(run.Assets)
	if err != nil {
		return fmt.Errorf("encoding assets: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO runs (id, machine, hash, source, assets, status, current_state, parent_run_id, created, updated)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.Machine, run.Hash, run.Source, string(assets), run.Status, run.CurrentState, run.ParentRunID, now, now)
	if err != nil {
		return fmt.Errorf("inserting run %s: %w", run.ID, err)
	}
	return nil
}

// UpdateRun updates status and current state.
func (s *SQLiteStore) UpdateRun(ctx context.Context, id, status, currentState string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, current_state = ?, updated = ? WHERE id = ?`,
		status, currentState, time.Now().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("updating run %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("run %q not found", id)
	}
	return nil
}

// runColumns is the canonical run projection shared by every run query, so
// scanRun's column order stays in lockstep with the SELECTs.
const runColumns = `id, machine, hash, source, assets, status, current_state, parent_run_id, created, updated`

// GetRun fetches one run.
func (s *SQLiteStore) GetRun(ctx context.Context, id string) (*Run, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	return scanRun(row)
}

// ListRuns returns top-level runs, most recent first. Parallel branch children
// (parent_run_id set) are excluded — they surface under their parent's detail
// view, not as standalone runs in `steps runs` or the web list.
func (s *SQLiteStore) ListRuns(ctx context.Context) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE parent_run_id = '' ORDER BY created DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing runs: %w", err)
	}
	return scanRuns(rows, "runs")
}

// ListRunsByStatus returns runs with the given status, oldest first (FIFO) so
// the dispatcher drains the queue in enqueue order.
func (s *SQLiteStore) ListRunsByStatus(ctx context.Context, status string) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE status = ? ORDER BY created ASC`, status)
	if err != nil {
		return nil, fmt.Errorf("listing runs by status %q: %w", status, err)
	}
	return scanRuns(rows, "runs by status")
}

// ListChildRuns returns a parent run's parallel branch children, oldest first.
func (s *SQLiteStore) ListChildRuns(ctx context.Context, parentID string) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE parent_run_id = ? ORDER BY created ASC`, parentID)
	if err != nil {
		return nil, fmt.Errorf("listing child runs of %q: %w", parentID, err)
	}
	return scanRuns(rows, "child runs")
}

// scanRuns drains a run-projection result set, closing it.
func scanRuns(rows *sql.Rows, what string) ([]*Run, error) {
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating %s: %w", what, err)
	}
	return out, nil
}

type scannable interface{ Scan(dest ...any) error }

func scanRun(row scannable) (*Run, error) {
	var r Run
	var created, updated int64
	var assets string
	err := row.Scan(&r.ID, &r.Machine, &r.Hash, &r.Source, &assets, &r.Status, &r.CurrentState, &r.ParentRunID, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("run not found")
		}
		return nil, fmt.Errorf("scanning run: %w", err)
	}
	err = json.Unmarshal([]byte(assets), &r.Assets)
	if err != nil {
		return nil, fmt.Errorf("decoding pinned assets: %w", err)
	}
	r.Created = time.UnixMilli(created)
	r.Updated = time.UnixMilli(updated)
	return &r, nil
}

// Append writes the event with the next sequence number for its run.
func (s *SQLiteStore) Append(ctx context.Context, ev *Event) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return 0, fmt.Errorf("encoding event data: %w", err)
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var seq int
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE run_id = ?`, ev.RunID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("computing next sequence for run %s: %w", ev.RunID, err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO events (run_id, seq, type, ts, data) VALUES (?, ?, ?, ?, ?)`,
		ev.RunID, seq, string(ev.Type), ev.Time.UnixMilli(), string(data))
	if err != nil {
		return 0, fmt.Errorf("inserting event: %w", err)
	}
	err = tx.Commit()
	if err != nil {
		return 0, fmt.Errorf("committing event: %w", err)
	}
	ev.Seq = seq
	return seq, nil
}

// Events returns the full journal for a run, in order.
func (s *SQLiteStore) Events(ctx context.Context, runID string) ([]*Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, type, ts, data FROM events WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("querying events for run %s: %w", runID, err)
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		ev := &Event{RunID: runID}
		var ts int64
		var data string
		err := rows.Scan(&ev.Seq, &ev.Type, &ts, &data)
		if err != nil {
			return nil, fmt.Errorf("scanning event: %w", err)
		}
		ev.Time = time.UnixMilli(ts)
		err = json.Unmarshal([]byte(data), &ev.Data)
		if err != nil {
			return nil, fmt.Errorf("decoding event data: %w", err)
		}
		out = append(out, ev)
	}
	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("iterating events for run %s: %w", runID, err)
	}
	return out, nil
}

// Close closes the database.
func (s *SQLiteStore) Close() error {
	err := s.db.Close()
	if err != nil {
		return fmt.Errorf("closing database: %w", err)
	}
	return nil
}
