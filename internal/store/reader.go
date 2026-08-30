package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
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
// From a Store it shares that handle's connection rather than opening its own:
// the same file, the same WAL, and no second handle to keep in step. From
// OpenReader it owns one, because there is no Store to borrow from — which is
// the only difference between the two, and what `owned` records so Close knows
// whether the connection is its to close.
//
// What a Reader never shares is the scoping, which is the whole point.
type Reader struct {
	db *sql.DB
	// owned is false for a Reader borrowing a Store's connection. Closing that
	// one would leave the Store holding a dead handle, and the Store's own
	// Close is what checkpoints the WAL.
	owned bool
}

// Reader returns an unscoped reader over the same file this handle is scoped
// to. Reading through it crosses pipelines by construction, so every method
// on it names which ones it wants.
func (s *Store) Reader() *Reader { return &Reader{db: s.db} }

// OpenReader opens a state file for cross-pipeline reading, with no pipeline
// to scope to and none to become.
//
// Every other way into this package registers the pipeline it was handed:
// OpenStore creates the directory, the file and a pipelines row, which is
// right for a command about to record something and wrong for one that is
// only asking. `steps runs --state shared.db` has no pipeline to name, so it
// has nothing to register — and a read command that left a database (or an
// invented pipeline) behind would be a surprising answer to a question about
// history.
//
// It refuses a file that is not there rather than creating one. The sqlite
// driver connects lazily, so an absent path otherwise becomes an empty
// database at the first query, and "no runs" and "no such file" are not the
// same answer.
func OpenReader(path string) (*Reader, error) {
	_, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("could not open state db %q: %w", path, err)
	}

	db, err := openDB(path)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()

	// Refused, never stamped: OpenStore blesses a version-0 file when it just
	// created it, and a reader never creates one, so any zero it sees belongs
	// to a build older than the check. Stamping here would be a write from a
	// command whose whole contract is that it only asks.
	found, err := readSchemaVersion(ctx, db, path)
	if err != nil {
		_ = db.Close()

		return nil, err
	}

	if found != schemaVersion {
		_ = db.Close()

		return nil, schemaVersionError(path, found)
	}

	return &Reader{db: db, owned: true}, nil
}

// ErrNoSuchPipeline is a name the state file has never heard of.
var ErrNoSuchPipeline = errors.New("no such pipeline in this state file")

// OpenExisting scopes a handle to a pipeline that is ALREADY in the file,
// rather than to one this call creates.
//
// OpenStore registers what it is handed, which is right for a command about
// to record something: `steps run new.yml` should not have to announce the
// pipeline first. It is wrong for a command that only asks, and the
// difference is not academic — `steps runs typo.yml` used to leave `typo` in
// the pipelines table forever, and answer "no job runs recorded", which is
// what a pipeline that has never run says too. A read that invents its own
// subject cannot tell you that you misspelled it.
//
// The handle it returns is an ordinary Store, deliberately: the ~60 scoped
// reads are already written and a read-only twin of them would be sixty
// chances to diverge. What differs is how the scope was obtained — resolved,
// never created.
func OpenExisting(path, pipelineName string) (*Store, error) {
	reader, err := OpenReader(path)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()

	var id int64

	err = reader.db.QueryRowContext(ctx,
		`SELECT id FROM pipelines WHERE name = ?`, pipelineName).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// Named alongside what the file does hold: the mistake this catches
		// is a name that is nearly right, and the fix is almost always
		// visible in the list. Read through the reader still open here, not a
		// second one.
		held, listErr := reader.names(ctx)

		_ = reader.Close()

		if listErr != nil {
			return nil, listErr
		}

		return nil, fmt.Errorf("%w: %s is not in %s, which holds: %s",
			ErrNoSuchPipeline, pipelineName, path, held)
	}

	if err != nil {
		_ = reader.Close()

		return nil, fmt.Errorf("could not resolve pipeline %q in %q: %w", pipelineName, path, err)
	}

	return &Store{db: reader.db, path: path, pipeline: pipelineName, pipelineID: id}, nil
}

// names is the "did you mean" half of the refusal above.
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

// Close releases the connection, if this Reader is the one that opened it.
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
