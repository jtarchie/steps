package store

import (
	"context"
	"errors"
)

// Reader reads ACROSS the pipelines in one database.
//
// Every read on a Store is scoped: the handle holds its pipeline and every
// method filters by it, which is what makes forgetting to scope impossible.
// That property is worth keeping exactly as it is, so a cross-pipeline read is
// a different TYPE rather than a nullable parameter threaded through the
// scoped methods — the alternative puts the footgun back, and puts it in the
// one place merkle hashing makes it dangerous.
//
// A Reader can therefore answer only questions that name their pipelines out
// loud. It cannot be handed to code expecting a Store, and there is no method
// here that silently means "all of them".
type Reader interface {
	// Pipelines is what this database actually holds.
	Pipelines(ctx context.Context) ([]PipelineRow, error)
	// RecentRuns interleaves the named pipelines' runs, newest first. A name
	// the database has never heard of contributes nothing rather than failing:
	// the caller asked about a set, not about each one.
	RecentRuns(ctx context.Context, pipelines []string, limit int) ([]CrossRunRow, error)
	// Close releases the connection, unless this Reader borrowed one from a
	// Store — in which case closing is the Store's to do.
	Close() error
}

// PipelineRow is one row of the pipelines table: what a state database
// actually holds, which is the question a shared one created and nothing
// asked.
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

// ErrNoState is a state database with no steps schema in it yet: one a writer
// is in the middle of creating, or a file somebody made with touch. Nothing
// has been recorded, which is an answer rather than a failure, and a driver
// distinguishes it from a version mismatch — telling an operator to delete the
// database their first run is this instant creating is the worst possible
// advice.
var ErrNoState = errors.New("state database has nothing recorded yet")

// ErrNoSuchPipeline is a name the state database has never heard of.
//
// It is the reason opening for reading RESOLVES a pipeline rather than
// registering one: a read that invents its own subject answers "no job runs
// recorded" to a typo, which is what a pipeline that has never run says too.
var ErrNoSuchPipeline = errors.New("no such pipeline in this state file")
