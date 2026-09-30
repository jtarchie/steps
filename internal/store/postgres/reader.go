package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jtarchie/steps/internal/store"
)

var _ store.Reader = (*Reader)(nil)

// Reader reads across every pipeline in one schema. From a Store it borrows
// that handle's pool; from OpenReader it owns a read-only one.
type Reader struct {
	db    *sql.DB
	owned bool
}

// Reader returns an unscoped reader over this handle's database.
func (s *Store) Reader() store.Reader { return &Reader{db: s.db} }

// OpenReader opens a database for cross-pipeline reading, registering
// nothing and creating nothing.
//
// Its sessions are default_transaction_read_only: the sqlite driver can only
// promise a reader never writes, and Postgres can hold it to that.
//
// A schema steps has never written is store.ErrNoState — "nothing recorded"
// is the same answer sqlite gives a file with no tables. A database that is
// not there at all is an error with advice, not ErrNoState: that is a typo in
// --db, or a database nobody created, and reading it as "no runs yet" would
// hide both.
func OpenReader(rawURL string) (*Reader, error) {
	conn, err := open(rawURL, true)
	if err != nil {
		return nil, err
	}

	err = checkExisting(context.Background(), conn)
	if err != nil {
		_ = conn.db.Close()

		return nil, err
	}

	return &Reader{db: conn.db, owned: true}, nil
}

// checkExisting refuses a schema that is absent or another build's, and
// writes nothing either way.
func checkExisting(ctx context.Context, conn connection) error {
	found, present, err := readSchemaVersion(ctx, conn.db)
	if err != nil {
		return fmt.Errorf("could not open state db %s: %w", conn.description, err)
	}

	if !present {
		return fmt.Errorf("%w: %s", store.ErrNoState, conn.description)
	}

	if found != schemaVersion {
		return schemaVersionError(conn, found)
	}

	return nil
}

// OpenExisting scopes a handle to a pipeline ALREADY in the database, rather
// than registering one.
//
// Unlike OpenReader its sessions may write: approving, answering and
// unpausing all resolve their pipeline this way and then record something.
// What it never does is create — a schema, a table, or the pipeline it was
// asked about.
func OpenExisting(rawURL, pipelineName string) (*Store, error) {
	conn, err := open(rawURL, false)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()

	err = checkExisting(ctx, conn)
	if err != nil {
		_ = conn.db.Close()

		return nil, err
	}

	var id int64

	err = conn.db.QueryRowContext(ctx,
		`SELECT id FROM pipelines WHERE name = $1`, pipelineName).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		held, listErr := (&Reader{db: conn.db}).names(ctx)

		_ = conn.db.Close()

		if listErr != nil {
			return nil, listErr
		}

		return nil, fmt.Errorf("%w: %s is not in %s, which holds: %s",
			store.ErrNoSuchPipeline, pipelineName, conn.description, held)
	}

	if err != nil {
		_ = conn.db.Close()

		return nil, fmt.Errorf("could not resolve pipeline %q in %s: %w", pipelineName, conn.description, err)
	}

	return &Store{db: conn.db, description: conn.description, pipeline: pipelineName, pipelineID: id}, nil
}

func (r *Reader) names(ctx context.Context) (string, error) {
	rows, err := r.Pipelines(ctx)
	if err != nil {
		return "", err
	}

	if len(rows) == 0 {
		return "nothing", nil
	}

	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}

	return strings.Join(names, ", "), nil
}

// Close releases the pool, if this Reader is the one that opened it.
func (r *Reader) Close() error {
	if !r.owned {
		return nil
	}

	err := r.db.Close()
	if err != nil {
		return fmt.Errorf("could not close state db: %w", err)
	}

	return nil
}

// Pipelines lists every pipeline in the schema, by name.
func (r *Reader) Pipelines(ctx context.Context) ([]store.PipelineRow, error) {
	return collect(ctx, r.db, "pipelines", `
		SELECT p.name, p.path, COALESCE(v.sha, ''), p.paused_at IS NOT NULL
		FROM pipelines p
		LEFT JOIN pipeline_revisions v ON v.id = p.current_revision_id
		ORDER BY p.name
	`, nil, func(rows *sql.Rows) (store.PipelineRow, error) {
		var row store.PipelineRow

		err := rows.Scan(&row.Name, &row.Path, &row.CurrentSHA, &row.Paused)

		return row, err //nolint:wrapcheck // collect wraps it with the query's own context
	})
}

// RecentRuns returns the newest runs across the NAMED pipelines, newest
// first; naming none asks for none.
func (r *Reader) RecentRuns(ctx context.Context, pipelines []string, limit int) ([]store.CrossRunRow, error) {
	if len(pipelines) == 0 {
		return nil, nil
	}

	return collect(ctx, r.db, "runs across pipelines", `
		SELECT p.name, `+runColumnsR+`
		FROM runs r
		JOIN pipelines p ON p.id = r.pipeline_id
		WHERE p.name = ANY($1::text[])
		ORDER BY r.started_at DESC, r.seq DESC
		LIMIT $2
	`, []any{pipelines, rowLimit(limit)}, func(rows *sql.Rows) (store.CrossRunRow, error) {
		var row store.CrossRunRow

		inner, err := scanRunRow(withLeadingDest{rows: rows, extra: []any{&row.Pipeline}})
		row.RunRow = inner

		return row, err
	})
}

// withLeadingDest lets a scanner that knows one column list read a row that
// has extra columns in front of it.
type withLeadingDest struct {
	rows  *sql.Rows
	extra []any
}

func (w withLeadingDest) Scan(dest ...any) error {
	return w.rows.Scan(append(append([]any{}, w.extra...), dest...)...) //nolint:wrapcheck // the caller names the row it was reading
}

// HasNothingRecorded reports a database that exists but holds no steps
// schema.
func HasNothingRecorded(rawURL string) bool {
	reader, err := OpenReader(rawURL)
	if err != nil {
		return errors.Is(err, store.ErrNoState)
	}

	_ = reader.Close()

	return false
}
