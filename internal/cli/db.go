package cli

// The one place a driver is chosen: a State names a sqlite file or a postgres
// url, and everything below internal/cli takes the store.Store opened here.

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/postgres"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// openState opens a state database for writing, registering the pipeline.
func openState(state State, name string) (store.Store, error) {
	if state.postgres() {
		return postgres.OpenStore(string(state), name) //nolint:wrapcheck // every caller names what it was opening
	}

	return sqlite.OpenStore(string(state), name) //nolint:wrapcheck // every caller names what it was opening
}

// openExistingState resolves a pipeline already in the database, creating
// nothing.
func openExistingState(state State, name string) (store.Store, error) {
	if state.postgres() {
		return postgres.OpenExisting(string(state), name) //nolint:wrapcheck // every caller names what it was opening
	}

	return sqlite.OpenExisting(string(state), name) //nolint:wrapcheck // every caller names what it was opening
}

// openStateReader opens a database for reading across its pipelines.
func openStateReader(state State) (store.Reader, error) {
	if state.postgres() {
		return postgres.OpenReader(string(state)) //nolint:wrapcheck // every caller names what it was opening
	}

	return sqlite.OpenReader(string(state)) //nolint:wrapcheck // every caller names what it was opening
}

// daemonIdentity is the id a daemon stamps on the machines it starts, minted into its database; one that cannot be read is logged and stamps nothing, which is how a daemon behaved before stamps, since load reports the database's real problem.
func daemonIdentity(ctx context.Context, state State) string {
	var (
		owner string
		err   error
	)

	if state.postgres() {
		owner, err = postgres.Identity(ctx, string(state))
	} else {
		owner, err = sqlite.Identity(ctx, string(state))
	}

	if err != nil {
		slog.ErrorContext(ctx, "web.identity_unreadable", "state", state, "error", err,
			"consequence", "machines this daemon starts are not marked as its own, so one a crash leaves running is not given back after a restart")

		return ""
	}

	return owner
}

// stateIsEmpty reports a database with nothing recorded — for sqlite, one
// whose file is not there yet counts. Asked BEFORE opening, so asking about
// history never creates the database it asks about.
func stateIsEmpty(state State) bool {
	if state.postgres() {
		return postgres.HasNothingRecorded(string(state))
	}

	_, err := os.Stat(string(state))
	if err != nil {
		return true
	}

	// A file with no schema in it is the same answer as no file: a writer
	// creates the database before it fills it in, so a reader arriving in
	// that window must not report the operator's brand new database as one
	// written by a different version of steps.
	return sqlite.HasNothingRecorded(string(state))
}

// warnPasswordInURL says that a password in --db is kept by every process
// listing and shell history that saw the command. Called once the logger is
// installed rather than as the flag parses, which is before any logger
// exists and would put a bare line on a daemon's slog-only terminal.
func warnPasswordInURL(raw string) {
	if postgres.HasPassword(raw) {
		slog.Warn("db.password_in_url",
			"why", "--db carries a password, which process listings and shell history keep; set PGPASSWORD, or use ~/.pgpass or PGSERVICEFILE, instead")
	}
}

// shellArg quotes a value for the command line a hint prints to be pasted.
// A url's `?` and `&` would otherwise glob and background the command, and a
// path with a space would split in two.
func shellArg(value string) string {
	if value != "" && strings.Trim(value, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:@%+=,-") == "" {
		return value
	}

	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
