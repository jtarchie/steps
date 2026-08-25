package store

import (
	"context"
	"database/sql"
)

// Reading ACROSS the pipelines in one state file.
//
// Every other read in this package is scoped: a Store holds a pipeline_id and
// each of its ~60 methods filters by it, which is what makes forgetting to
// scope impossible. That property is worth keeping exactly as it is, so a
// cross-pipeline read is a different TYPE rather than a nullable parameter
// threaded through the existing methods — the alternative puts the footgun
// back, and puts it in the one place merkle hashing makes it dangerous
// (HashNode folds in kind/content/parent but not the pipeline, so two
// pipelines with a job named `build` over an identical task hash the same).
//
// A Reader can therefore answer only questions that name their pipelines out
// loud. It cannot be handed to code expecting a Store, and there is no method
// here that silently means "all of them".

// PipelineRow is one row of the pipelines table: what a state file actually
// holds, which is the question `--state shared.db` created and nothing asked.
type PipelineRow struct {
	Name string
	Path string
}

// CrossRunRow is a run plus the pipeline it belongs to. The pipeline is not
// optional: a feed that spans pipelines and cannot say which one a row came
// from is showing runs it has no route to.
type CrossRunRow struct {
	RunRow

	Pipeline string
}

// Reader reads across every pipeline in one state file.
//
// It shares the Store's connection rather than opening its own: the same file,
// the same WAL, and no second handle to keep in step. What it does not share
// is the scoping, which is the whole point.
type Reader struct{ db *sql.DB }

// Reader returns an unscoped reader over the same file this handle is scoped
// to. Reading through it crosses pipelines by construction, so every method
// on it names which ones it wants.
func (s *Store) Reader() *Reader { return &Reader{db: s.db} }

// Pipelines lists every pipeline recorded in the file, by name.
//
// Ordered by name so a listing is stable rather than in the order rows
// happened to be created — a page whose entries move between reloads reads as
// a bug even when nothing changed.
func (r *Reader) Pipelines(ctx context.Context) ([]PipelineRow, error) {
	return collect(ctx, r.db, "pipelines", `
		SELECT name, path
		FROM pipelines
		ORDER BY name
	`, nil, func(rows *sql.Rows) (PipelineRow, error) {
		var row PipelineRow

		err := rows.Scan(&row.Name, &row.Path)

		return row, err //nolint:wrapcheck // collect wraps it with the query's own context
	})
}

// RecentRuns returns the newest runs across the NAMED pipelines, newest
// first.
//
// The names are a filter in SQL, not applied to the result, and the
// difference is load-bearing: a state file may hold a pipeline this process
// does not serve, and fetching a limit and then dropping those rows would
// show fewer runs the busier the unserved pipeline is. Naming none asks for
// none — an empty served list is a configuration worth reporting, never a
// wildcard.
func (r *Reader) RecentRuns(ctx context.Context, pipelines []string, limit int) ([]CrossRunRow, error) {
	if len(pipelines) == 0 {
		return nil, nil
	}

	args := make([]any, 0, len(pipelines)+1)
	for _, name := range pipelines {
		args = append(args, name)
	}

	args = append(args, limit)

	// One ordering pass over the joined rows rather than a query per pipeline
	// merged afterwards: a merge would have to fetch `limit` from each to be
	// correct, and gets the interleaving wrong the moment it does not.
	query := `
		SELECT p.name, ` + runColumnsR + `
		FROM runs r
		JOIN pipelines p ON p.id = r.pipeline_id
		WHERE p.name IN (` + placeholders(len(pipelines)) + `)
		ORDER BY r.started_at DESC, r.rowid DESC
		LIMIT ?
	`

	return collect(ctx, r.db, "runs across pipelines", query, args, func(rows *sql.Rows) (CrossRunRow, error) {
		var row CrossRunRow

		// Through scanRunRow rather than around it: runColumns exists because
		// a second call site that read its own subset made Replayed() answer
		// differently depending on who loaded the row. Selecting the same
		// list and then scanning it by hand would reintroduce exactly that,
		// one argument order at a time.
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
