package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jtarchie/steps/internal/store"
)

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

// errText is store.ErrText for TEXT that refuses what clean strips.
func errText(err error) any {
	if err == nil {
		return nil
	}

	return boundedError(err.Error())
}

// boundedError is store.BoundedError after clean, which must run first: the
// cap counts the bytes Postgres will actually store.
func boundedError(text string) string {
	return store.BoundedError(clean(text))
}

// eventText cleans and caps an event's text columns.
func eventText(text string) string {
	return store.TruncateUTF8(clean(text), store.MaxEventTextBytes)
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
func textArray[S ~string](values []S) []string {
	array := make([]string, len(values))
	for i, value := range values {
		array[i] = string(value)
	}

	return array
}

func scanString[S ~string](rows *sql.Rows) (S, error) {
	var value S

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
