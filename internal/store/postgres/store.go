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
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	db          *sql.DB
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
	conn, err := open(rawURL, false)
	if err != nil {
		return nil, err
	}

	id, err := initDB(context.Background(), conn, pipelineName)
	if err != nil {
		_ = conn.db.Close()

		return nil, err
	}

	return &Store{db: conn.db, description: conn.description, pipeline: pipelineName, pipelineID: id}, nil
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

	// ponytail: one pool per handle, so a daemon serving N pipelines holds up
	// to 4N connections; share one pool per URL if that ever meets
	// max_connections.
	db.SetMaxOpenConns(4)
	// A server restart or a NAT dropping an idle flow leaves a pool holding
	// dead connections; recycling them bounds how long that lasts.
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)

	return connection{db: db, description: describeConfig(config, schema), schema: schema}, nil
}

// connConfig turns a --db URL into a connection config pinned to this
// driver's schema.
//
// search_path is set to that schema and NOTHING else, so an unqualified name
// can only ever resolve to steps' own table — never to a same-named table in
// public, and never to one somebody planted earlier in the path. A URL's
// search_path chooses the schema (its first element), it does not widen it.
//
// The error never carries the URL: pgconn's own echoes it redacted
// best-effort, and a URL that failed to parse is exactly the one whose
// password a redactor may not find.
func connConfig(rawURL string) (*pgx.ConnConfig, string, error) {
	config, err := pgx.ParseConfig(rawURL)
	if err != nil {
		return nil, "", errors.New("could not parse the postgres URL given to --db; check its syntax (the URL is not repeated here because it may carry a password)")
	}

	schema := DefaultSchema

	if path := config.RuntimeParams["search_path"]; path != "" {
		first, _, _ := strings.Cut(path, ",")
		first = strings.TrimSpace(first)

		if unquoted, ok := strings.CutPrefix(first, `"`); ok {
			first = strings.ReplaceAll(strings.TrimSuffix(unquoted, `"`), `""`, `"`)
		}

		if first != "" {
			schema = first
		}
	}

	// A url copied from another application's config often says
	// search_path=public, or libpq's default "$user", public — and either
	// would put steps' tables beside that application's.
	if schema == "public" || schema == "$user" {
		return nil, "", fmt.Errorf("--db names schema %s: steps keeps its tables out of %s; set search_path to a schema of its own, or leave it out for %q", schema, schema, DefaultSchema)
	}

	config.RuntimeParams["search_path"] = quote(schema)
	// Timestamps come back in the session's zone; UTC is what every reader
	// here compares them against.
	config.RuntimeParams["TimeZone"] = "UTC"

	// pgx waits forever by default, and the event sink and Close run on a
	// context nobody cancels: a blackholed host would hang the open for good.
	if config.ConnectTimeout == 0 {
		config.ConnectTimeout = 10 * time.Second
	}

	return config, schema, nil
}

func quote(identifier string) string { return pgx.Identifier{identifier}.Sanitize() }

// describeConfig is Description: the URL rebuilt from what the config
// resolved, which carries no password by construction, plus the schema —
// two schemas in one database are two state databases.
func describeConfig(config *pgx.ConnConfig, schema string) string {
	described := url.URL{Scheme: "postgres", Path: "/" + config.Database}
	if config.User != "" {
		described.User = url.User(config.User)
	}

	query := url.Values{}

	if strings.HasPrefix(config.Host, "/") {
		query.Set("host", config.Host)
	} else {
		described.Host = net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port)))
	}

	query.Set("search_path", schema)
	described.RawQuery = query.Encode()

	return described.String()
}

// Redact is a --db URL safe to print: the userinfo password and the
// password= and sslpassword= parameters removed, everything else kept.
func Redact(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		// Unparseable means the boundaries a redactor needs are unknown.
		return "postgres://(unparseable URL)"
	}

	if parsed.User != nil {
		parsed.User = url.User(parsed.User.Username())
	}

	query := parsed.Query()
	if query.Has("password") || query.Has("sslpassword") {
		query.Del("password")
		query.Del("sslpassword")
		parsed.RawQuery = query.Encode()
	}

	return parsed.String()
}

// HasPassword reports a URL carrying its own password, which a process list
// and a shell history both keep.
func HasPassword(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}

	_, inUserinfo := parsed.User.Password()
	query := parsed.Query()

	return inUserinfo || query.Get("password") != "" || query.Get("sslpassword") != ""
}

// describe turns the failures an operator can fix into what to do about them.
// Everything else passes through as pgx reported it, which names the user and
// database and never the password.
func describe(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}

	switch pgErr.Code {
	case "3D000":
		return fmt.Errorf("%w — steps creates its schema but never the database; create it with `createdb` first", err)
	case "28P01", "28000":
		return fmt.Errorf("%w — set PGPASSWORD, or use ~/.pgpass or PGSERVICEFILE, rather than putting a password in --db", err)
	}

	return err
}

// initDB creates the schema on first sight, refuses one another build wrote,
// and registers the pipeline — all in one transaction, so a refused or failed
// open leaves nothing behind (Postgres DDL is transactional).
//
// The DDL runs only when the schema is ABSENT. A matching schema costs one
// read, not sixty statements over the network, and needs no CREATE privilege,
// so a role that may only use the tables can open it.
//
// The advisory lock serializes two first opens: without it both see no
// schema_version and both run the DDL, and the loser fails on a duplicate.
func initDB(ctx context.Context, conn connection, pipelineName string) (int64, error) {
	var id int64

	err := inTx(ctx, conn.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, 0)`, classSchema)
		if err != nil {
			return err //nolint:wrapcheck // inTx's caller names the database
		}

		found, present, err := readSchemaVersion(ctx, tx, conn.schema)
		if err != nil {
			return err
		}

		switch {
		case !present:
			err = createSchema(ctx, tx, conn.schema)
		case found != schemaVersion:
			err = schemaVersionError(conn, found)
		}

		if err != nil {
			return err
		}

		id, err = registerPipeline(ctx, tx, pipelineName)

		return err
	})
	if err != nil {
		return 0, fmt.Errorf("could not open state db %s: %w", conn.description, describe(err))
	}

	return id, nil
}

func createSchema(ctx context.Context, tx *sql.Tx, schemaName string) error {
	var exists bool

	err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schemaName).Scan(&exists)
	if err != nil {
		return fmt.Errorf("could not read the schemas: %w", err)
	}

	if !exists {
		_, err = tx.ExecContext(ctx, `CREATE SCHEMA `+quote(schemaName))
		if err != nil {
			return fmt.Errorf("could not create schema %s: %w", schemaName, err)
		}
	}

	_, err = tx.ExecContext(ctx, schema)
	if err != nil {
		return fmt.Errorf("could not create the tables: %w", err)
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES ($1)`, schemaVersion)
	if err != nil {
		return fmt.Errorf("could not stamp the schema version: %w", err)
	}

	return nil
}

// readSchemaVersion reports the stamp, and whether there is one at all. An
// older steps schema cannot lack the table — it was there from the first —
// so absent means this schema has never been written.
//
// It also refuses a session the pinned search_path never reached. A pooler
// told to drop the startup parameter (PgBouncer's ignore_startup_parameters,
// the usual answer to its "unsupported startup parameter") leaves the
// server's default "$user", public — and every unqualified name here would
// then create and write steps' tables in public, beside another
// application's. Same round trip as the stamp check, so it costs nothing.
func readSchemaVersion(ctx context.Context, db executor, schemaName string) (int, bool, error) {
	var (
		present bool
		path    string
	)

	err := db.QueryRowContext(ctx,
		`SELECT to_regclass('schema_version') IS NOT NULL, current_setting('search_path')`).Scan(&present, &path)
	if err != nil {
		return 0, false, describe(err)
	}

	if path != quote(schemaName) {
		return 0, false, fmt.Errorf("the session's search_path is %q, not %s: something between steps and the server dropped the one steps sets (a pooler's ignore_startup_parameters?), and its tables would land in the wrong schema; add search_path to PgBouncer's track_extra_parameters instead", path, quote(schemaName))
	}

	if !present {
		return 0, false, nil
	}

	var found int

	err = db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&found)
	if err != nil {
		return 0, false, fmt.Errorf("could not read the schema version: %w", err)
	}

	return found, true, nil
}

func schemaVersionError(conn connection, found int) error {
	return fmt.Errorf("%w: %s is at schema %d and this steps writes %d — there is no upgrade path; `DROP SCHEMA %s CASCADE` and run again, which loses steps' run history and cache in that schema and nothing else",
		store.ErrSchemaVersion, conn.description, found, schemaVersion, quote(conn.schema))
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
	err := s.db.Close()
	if err != nil {
		return fmt.Errorf("could not close state db: %w", err)
	}

	return nil
}

// executor is what a query needs from either a pool or a transaction.
type executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// write runs fn in a transaction holding this pipeline's advisory lock.
//
// sqlite serialized every write transaction for free — one writer per file,
// taken at BEGIN — and several of this package's statements were written
// leaning on that: a check_order from a MAX just read, a claim's counts of
// what is already running, a prune's view of what survives. Under READ
// COMMITTED those are races (a duplicate check_order, max_in_flight broken by
// two claimers each counting the other's row as not yet there). The lock
// gives each pipeline its single writer back, for exactly the transactions
// that read before they write. A plain keyed upsert does not take it: the
// database already keeps that one atomic.
//
// The lock outlives its client if the client vanishes mid-transaction: the
// server notices a dead peer only through TCP keepalive, hours by default,
// and every write of the pipeline waits that long behind a backend nobody
// is driving. lockHolderIdleLimit ends such a session, and nothing a live
// holder does between statements comes near it.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return inTx(ctx, s.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`SELECT set_config('idle_in_transaction_session_timeout', $3, true), pg_advisory_xact_lock($1, $2::int)`,
			classPipeline, s.pipelineID, lockHolderIdleLimit)
		if err != nil {
			return fmt.Errorf("could not lock pipeline %q: %w", s.pipeline, err)
		}

		return fn(tx)
	})
}

// inTx commits fn's work, or rolls it back when fn fails.
func inTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return describe(err)
	}

	defer func() { _ = tx.Rollback() }()

	err = fn(tx)
	if err != nil {
		return err
	}

	return tx.Commit() //nolint:wrapcheck // every caller names what it was committing
}

// now is the write timestamp: UTC, and truncated to the microsecond
// Postgres keeps, so a time written and a time compared against agree.
func now() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// sortableNano is the sqlite driver's layout for the string fields that
// carried it, so a row reads the same whichever driver wrote it.
const sortableNano = "2006-01-02T15:04:05.000000000Z07:00"

// stamp renders a nullable timestamp for the row types that carry a string:
// "" for NULL, as sqlite's COALESCE did.
func stamp(value sql.NullTime, layout string) string {
	if !value.Valid {
		return ""
	}

	return value.Time.UTC().Format(layout)
}

// utc reads a nullable timestamp as the zero Time for NULL.
func utc(value sql.NullTime) time.Time {
	if !value.Valid {
		return time.Time{}
	}

	return value.Time.UTC()
}

// clean makes text Postgres will store. TEXT refuses a NUL byte and invalid
// UTF-8 outright, where sqlite kept both — and most of what lands here comes
// from outside steps (a command's output, a model's answer, an error), while
// the event sink only WARNS on a failed insert. Unsanitized, one stray byte
// in a task's output would silently drop that event.
func clean(value string) string {
	if utf8.ValidString(value) && !strings.ContainsRune(value, 0) {
		return value
	}

	return strings.ReplaceAll(strings.ToValidUTF8(value, "�"), "\x00", "")
}

// nullable stores an empty string as NULL, for the reasons the sqlite
// driver's nullable gives.
func nullable(value string) any {
	if value == "" {
		return nil
	}

	return clean(value)
}

func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}

	return clean(string(b))
}

// errText renders an error for a nullable error column: NULL when there was
// none, and never more than MaxStoredErrorBytes.
func errText(err error) any {
	if err == nil {
		return nil
	}

	return boundedError(err.Error())
}

// boundedError caps a message at MaxStoredErrorBytes, keeping the head.
func boundedError(text string) string {
	text = clean(text)
	if len(text) <= store.MaxStoredErrorBytes {
		return text
	}

	const notice = "… [truncated]"

	return truncateUTF8(text, store.MaxStoredErrorBytes-len(notice)) + notice
}

// eventText cleans and caps an event's text columns.
func eventText(text string) string {
	return truncateUTF8(clean(text), store.MaxEventTextBytes)
}

// rowLimit turns "zero means no limit" into LIMIT NULL, which Postgres reads
// as no limit at all.
func rowLimit(limit int) any {
	if limit <= 0 {
		return nil
	}

	return limit
}

// byJob narrows a listing to one job, or leaves it whole for an empty name —
// spliced in, because `($2 = ” OR job_name = $2)` is a predicate no index
// can serve.
func byJob(jobName string, args []any) (string, []any) {
	if jobName == "" {
		return "", args
	}

	args = append(args, jobName)

	return " AND job_name = $" + strconv.Itoa(len(args)), args
}

// next is the placeholder for the argument about to be appended to args.
func next(args []any) string { return "$" + strconv.Itoa(len(args)+1) }

// textArray binds a list as one text[] argument. Never NULL: `= ANY(NULL)`
// and unnest(NULL) happen to answer like an empty list, but only by accident.
func textArray(values []string) []string {
	if values == nil {
		return []string{}
	}

	return values
}

func scanString(rows *sql.Rows) (string, error) {
	var value string

	return value, rows.Scan(&value) //nolint:wrapcheck // collect wraps with the thing being read
}

func scanPair(rows *sql.Rows) ([2]string, error) {
	var pair [2]string

	return pair, rows.Scan(&pair[0], &pair[1]) //nolint:wrapcheck // collect wraps with the thing being read
}

// collect runs a query and decodes every row through scan.
func collect[T any](ctx context.Context, db executor, what, query string, args []any, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", what, err)
	}

	defer func() { _ = rows.Close() }()

	var out []T

	for rows.Next() {
		item, scanErr := scan(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("could not read %s: %w", what, scanErr)
		}

		out = append(out, item)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", what, err)
	}

	return out, nil
}

// truncateUTF8 cuts s to at most a byte limit without splitting a rune.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	cut := limit
	for cut > 0 && cut > limit-(utf8.UTFMax-1) && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut]
}
