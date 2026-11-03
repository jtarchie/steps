package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

func nullableString(b []byte) any {
	if len(b) == 0 {
		return nil
	}

	return string(b)
}

// rowLimit turns this repo's "zero means no limit" convention into the LIMIT
// sqlite wants for it, which is -1. Without it a caller asking for everything
// has to pass a number it invented, and the number it invents is the bug.
func rowLimit(limit int) int {
	if limit <= 0 {
		return -1
	}

	return limit
}

// byJob narrows a listing to one job, or leaves it whole for an empty name. Spliced in rather than written `(? = ” OR job_name = ?)`, which no index can serve: every call sorted the pipeline's whole history to return twenty rows.
func byJob(jobName string, args []any) (string, []any) {
	if jobName == "" {
		return "", args
	}

	return " AND job_name = ?", append(args, jobName)
}

// jsonList binds a list as ONE argument, read back with `IN (SELECT value FROM json_each(?))`. sqlite has no array binding, and the alternative — a generated "?,?,?" list — needed chunking under the bind-variable limit at every call site.
func jsonList[S ~string](values []S) string {
	if values == nil {
		// json_each('null') yields one NULL row rather than none, so an empty list must be an empty array.
		return "[]"
	}

	encoded, _ := json.Marshal(values) //nolint:errchkjson // a list of strings cannot fail to marshal

	return string(encoded)
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
//
// It exists because the alternative — query, defer Close, loop, scan, check
// rows.Err — is fifteen lines of identical bookkeeping at every list query in this
// package, and the last of those five steps is
// the one that is quietly forgotten. what names the thing being read, for the
// error message.
func collect[T any](ctx context.Context, db *sql.DB, what, query string, args []any, scan func(*sql.Rows) (T, error)) ([]T, error) {
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
