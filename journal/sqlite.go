package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, CGO-free
)

// SQLiteStore is the default durable Store.
type SQLiteStore struct {
	db *sql.DB
}

// OpenSQLite opens (and migrates) the journal database at path.
func OpenSQLite(path string) (*SQLiteStore, error) {
	// modernc.org/sqlite registers as "sqlite". Busy timeout keeps
	// concurrent CLI invocations (run + runs list) from erroring.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	s := &SQLiteStore{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS runs (
    id            TEXT PRIMARY KEY,
    machine       TEXT NOT NULL,
    hash          TEXT NOT NULL,
    yaml          BLOB NOT NULL,
    status        TEXT NOT NULL,
    current_state TEXT NOT NULL DEFAULT '',
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
);`)
	return err
}

// CreateRun inserts a new run row.
func (s *SQLiteStore) CreateRun(ctx context.Context, run *Run) error {
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (id, machine, hash, yaml, status, current_state, created, updated)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.Machine, run.Hash, run.YAML, run.Status, run.CurrentState, now, now)
	return err
}

// UpdateRun updates status and current state.
func (s *SQLiteStore) UpdateRun(ctx context.Context, id, status, currentState string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, current_state = ?, updated = ? WHERE id = ?`,
		status, currentState, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("run %q not found", id)
	}
	return nil
}

// GetRun fetches one run.
func (s *SQLiteStore) GetRun(ctx context.Context, id string) (*Run, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, machine, hash, yaml, status, current_state, created, updated FROM runs WHERE id = ?`, id)
	return scanRun(row)
}

// ListRuns returns all runs, most recent first.
func (s *SQLiteStore) ListRuns(ctx context.Context) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, machine, hash, yaml, status, current_state, created, updated FROM runs ORDER BY created DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type scannable interface{ Scan(dest ...any) error }

func scanRun(row scannable) (*Run, error) {
	var r Run
	var created, updated int64
	if err := row.Scan(&r.ID, &r.Machine, &r.Hash, &r.YAML, &r.Status, &r.CurrentState, &created, &updated); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("run not found")
		}
		return nil, err
	}
	r.Created = time.UnixMilli(created)
	r.Updated = time.UnixMilli(updated)
	return &r, nil
}

// Append writes the event with the next sequence number for its run.
func (s *SQLiteStore) Append(ctx context.Context, ev *Event) (int, error) {
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return 0, err
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var seq int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE run_id = ?`, ev.RunID).Scan(&seq); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (run_id, seq, type, ts, data) VALUES (?, ?, ?, ?, ?)`,
		ev.RunID, seq, string(ev.Type), ev.Time.UnixMilli(), string(data)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	ev.Seq = seq
	return seq, nil
}

// Events returns the full journal for a run, in order.
func (s *SQLiteStore) Events(ctx context.Context, runID string) ([]*Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, type, ts, data FROM events WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		ev := &Event{RunID: runID}
		var ts int64
		var data string
		if err := rows.Scan(&ev.Seq, &ev.Type, &ts, &data); err != nil {
			return nil, err
		}
		ev.Time = time.UnixMilli(ts)
		if err := json.Unmarshal([]byte(data), &ev.Data); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// Close closes the database.
func (s *SQLiteStore) Close() error { return s.db.Close() }
