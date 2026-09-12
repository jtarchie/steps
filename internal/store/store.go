// Package store is the contract steps holds its state database to: the row
// types every driver returns, the errors every driver raises, and the Store
// interface every driver implements.
//
// It runs no SQL and opens no connection. A driver — internal/store/sqlite
// today — owns its schema, its version stamp and its queries end to end, so
// each can use what its database is good at rather than the intersection of
// two. What keeps them honest with each other is this file plus the tests
// written against it.
//
// Only internal/cli names a driver; every other package takes a Store it was
// handed.
package store

import (
	"context"
	"errors"
)

// Meta is the handle itself: what it is scoped to, what it is backed by, and
// how it is let go. Every other facet is a table; this one is the identity the
// tables are read through.
type Meta interface {
	// Pipeline is the name this handle is scoped to.
	Pipeline() string

	// Description identifies the database behind this handle, in a form safe
	// to print: the file path for SQLite, a credential-free URL for a network
	// database. It is stable, so two handles reporting the same description
	// share a database — which is how a caller groups handles before asking
	// one Reader about several pipelines at once.
	Description() string

	// SetSourcePath records where this pipeline's YAML lives, so a reader of
	// the database can tell two checkouts apart. Only a command that LOADED
	// the pipeline calls it.
	SetSourcePath(ctx context.Context, source string) error

	// Reader reads across every pipeline in this database, unscoped by
	// construction.
	Reader() Reader

	// Close releases the connection, after whatever reclaim the driver owes
	// for what retention deleted. A handle obtained by RESOLVING a pipeline
	// rather than registering one must not write here: a command whose whole
	// contract is that it only asks changes no bytes.
	Close() error
}

// Store is one pipeline's view of the state database: every facet at once.
//
// The handle IS the scope, and that is the property the interface exists to
// preserve: a state file (or a Postgres database) may hold several pipelines,
// and no method here takes a pipeline name. A caller cannot forget to pass one
// and cannot pass the wrong one. It matters most for the cache — a merkle hash
// folds in kind, content and parent but NOT the pipeline, so two pipelines each
// with a job named `build` over an identical task hash the same, and an
// unscoped skip index would let the second one skip work it never did.
//
// `steps web` serving three pipelines from one database holds three of these.
// Reading ACROSS them is Reader, which is a different type for the same reason.
//
// It is a composition and NOTHING else — facets_test.go holds it to that.
// The facets are the aggregates the files here are already cut along, and they
// exist so a consumer can name the parts of the database it touches: agent
// takes Cache, Usage and Questions, trigger's poll takes Queue and Versions,
// and only what runs a whole build — pipeline and web — needs
// all of it. A method declared
// straight on Store would belong to no aggregate, so nobody could ask for it
// by name and a second driver would have no facet to write it under.
type Store interface {
	Meta
	Runs
	Cache
	Blobs
	Versions
	Queue
	Approvals
	Questions
	Placements
	Usage
	Events
	Revisions
	Control
	Pruning
}

// ErrSchemaVersion is a database some other build of steps wrote.
//
// There is no migration path, deliberately: a driver refuses a schema it
// cannot write rather than upgrading it, and the answer is to delete the
// database. What this exists for is that somebody is TOLD — an unrecognised
// schema used to open fine, lack a column, fail every insert naming it, and
// leave a green build that had recorded nothing.
var ErrSchemaVersion = errors.New("the state database was written by a different version of steps")

// MaxStoredErrorBytes bounds the error text any column here keeps.
//
// A stored error is unbounded by default, and one of them is routinely enormous:
// a failing check or task reports "command %q failed", where the command is the
// whole generated shell script — about 1.3KB for the built-in git check, with
// its comments. That message is exactly right in a terminal and far too big to
// keep a copy of per failure, forever. A remote that is down at a one-minute
// poll wrote that same 1.3KB every minute.
//
// 2KB keeps the head of any real message — the part naming what failed — while
// making the worst case a rounding error rather than the largest thing a failing
// watch produces.
const MaxStoredErrorBytes = 2 * 1024
