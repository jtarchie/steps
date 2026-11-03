// Package postgres is the Postgres driver for the state database: its own
// schema and its own SQL behind internal/store's contract, proven by the same
// conformance suite as the sqlite driver.
//
// Where the two differ it is on purpose, each using what its database is good
// at: an identity column for insertion order where sqlite has rowid, FOR
// UPDATE SKIP LOCKED for the claim, timestamptz where sqlite stores sortable
// text, and advisory locks where sqlite's single writer serialized everything
// for free (see write).
//
// Only internal/cli constructs one; every other package takes the store.Store
// it was handed.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/jtarchie/steps/internal/store"
)

var _ store.Store = (*Store)(nil)

// DefaultSchema is where steps keeps its tables unless the URL's search_path
// names another. Never public: a database the operator already runs has its
// own tables there, and CREATE TABLE IF NOT EXISTS would silently adopt one
// that happened to be called runs.
const DefaultSchema = "steps"

// Advisory-lock classes, the first key of pg_advisory_xact_lock(int, int).
// Lock keys are per DATABASE, not per schema, so two steps schemas in one
// database share them — which only ever over-serializes.
const (
	classSchema   = 7301
	classPipeline = 7302
	classContent  = 7303
)

// lockHolderIdleLimit is idle_in_transaction_session_timeout for a
// transaction holding the pipeline lock; see write.
const lockHolderIdleLimit = "60s"

// Store is one pipeline's handle on a Postgres state database. The handle is
// the scope, as it is for sqlite: pipelineID is in every query's predicate.
type Store struct {
	db *sql.DB
	// shared is the per-URL pool db came from (see pools.go), nil for a
	// handle with a reader's connection of its own (OpenExisting).
	shared      *pool
	released    atomic.Bool
	description string
	pipeline    string
	pipelineID  int64
}

// OpenStore connects, creates this driver's schema on first sight, and scopes
// the handle to the named pipeline — registering it if it is new.
//
// steps never creates the DATABASE: that is the operator's, with its owner,
// encoding and backups. The schema inside it is steps'.
func OpenStore(rawURL, pipelineName string) (*Store, error) {
	held, err := acquirePool(rawURL)
	if err != nil {
		return nil, err
	}

	id, err := initDB(context.Background(), held.connection, pipelineName)
	if err != nil {
		if held.release() {
			_ = held.db.Close()
		}

		return nil, err
	}

	return &Store{db: held.db, shared: held, description: held.description, pipeline: pipelineName, pipelineID: id}, nil
}

// connection is an open pool and what it is a pool on.
type connection struct {
	db          *sql.DB
	description string
	schema      string
}

// open parses the URL and opens a pool on it; nothing is dialled until the
// first query.
func open(rawURL string, readOnly bool) (connection, error) {
	config, schema, err := connConfig(rawURL)
	if err != nil {
		return connection{}, err
	}

	if readOnly {
		config.RuntimeParams["default_transaction_read_only"] = "on"
	}

	db := stdlib.OpenDB(*config)

	// A reader's own pool; a writable handle's is widened and shared (see pools.go).
	sizePool(db, 4)
	// A server restart or a NAT dropping an idle flow leaves a pool holding
	// dead connections; recycling them bounds how long that lasts.
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)

	return connection{db: db, description: describeConfig(config, schema), schema: schema}, nil
}

// sizePool bounds a pool and keeps every connection it opens: database/sql's
// default keeps two idle, so a burst past that closed the rest and the next
// burst redialled them — TCP, TLS and auth each time.
func sizePool(db *sql.DB, conns int) {
	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(conns)
}

func registerPipeline(ctx context.Context, db executor, name string) (int64, error) {
	if name == "" {
		return 0, errors.New("a state store needs a pipeline name; pass --name or let it default to the pipeline file's base name")
	}

	var id int64

	// RETURNING answers only for a new row; an existing one is read after,
	// which also sees a row a concurrent open committed while this insert
	// waited on it.
	err := db.QueryRowContext(ctx, `
		INSERT INTO pipelines (name, path) VALUES ($1, '')
		ON CONFLICT (name) DO NOTHING
		RETURNING id
	`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = db.QueryRowContext(ctx, `SELECT id FROM pipelines WHERE name = $1`, name).Scan(&id)
	}

	if err != nil {
		return 0, fmt.Errorf("could not register pipeline %q: %w", name, err)
	}

	return id, nil
}

// SetSourcePath records where this pipeline's YAML lives.
func (s *Store) SetSourcePath(ctx context.Context, source string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pipelines SET path = $1 WHERE id = $2`, clean(source), s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not record the source path of pipeline %q: %w", s.pipeline, err)
	}

	return nil
}

// Pipeline is the name this handle is scoped to.
func (s *Store) Pipeline() string { return s.pipeline }

// Description is the database and schema behind this handle, with no
// credential in it.
func (s *Store) Description() string { return s.description }

// Close releases the pool. There is nothing to compact: autovacuum does what
// sqlite's Close does by hand.
func (s *Store) Close() error { return s.Release() }

// Release is Close; the sqlite driver distinguishes them, this one has no
// reclaim to skip.
func (s *Store) Release() error {
	// Once however often it is called, and only the URL's last holder closes the pool.
	if s.released.Swap(true) || (s.shared != nil && !s.shared.release()) {
		return nil
	}

	err := s.db.Close()
	if err != nil {
		return fmt.Errorf("could not close state db: %w", err)
	}

	return nil
}
