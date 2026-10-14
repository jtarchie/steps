package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// Accounts is the daemon-level handle on the people who sign in: sessions now, beside the pipelines rather than in one.
type Accounts struct {
	held *pools
	path string
}

var _ store.Accounts = (*Accounts)(nil)

// OpenAccounts opens the accounts handle on a state file, creating the file and its schema the way a first OpenStore would.
func OpenAccounts(path string) (*Accounts, error) {
	err := os.MkdirAll(filepath.Dir(path), 0o750)
	if err != nil {
		return nil, fmt.Errorf("could not create state directory for %q: %w", path, err)
	}

	err = createPrivate(path)
	if err != nil {
		return nil, err
	}

	held, err := acquirePools(path)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()

	err = func() error {
		tx, err := held.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("could not open state db %q: %w", path, err)
		}

		defer func() { _ = tx.Rollback() }()

		err = initSchema(ctx, tx, path)
		if err != nil {
			return err
		}

		return tx.Commit()
	}()
	if err != nil {
		if held.release() {
			_ = held.reads.Close()
			_ = held.db.Close()
		}

		return nil, fmt.Errorf("could not open accounts in %q: %w", path, err)
	}

	return &Accounts{held: held, path: path}, nil
}

// Close lets go of the file's pools, closing them if nothing else holds them.
func (a *Accounts) Close() error {
	if a.held.release() {
		return errors.Join(a.held.reads.Close(), a.held.db.Close())
	}

	return nil
}

// CreateSession records a signed-in browser by its token's digest, and sweeps sessions that ended, which is the table's only bound: what is left is one row per browser still signed in.
func (a *Accounts) CreateSession(ctx context.Context, session store.Session) error {
	now := nowNano()

	_, err := a.held.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ? OR revoked_at IS NOT NULL`, now)
	if err != nil {
		return fmt.Errorf("could not sweep ended sessions: %w", err)
	}

	_, err = a.held.db.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, actor, created_at, expires_at) VALUES (?, ?, ?, ?)
	`, session.TokenHash, session.Actor, session.CreatedAt.UTC().Format(sortableNano), session.ExpiresAt.UTC().Format(sortableNano))
	if err != nil {
		return fmt.Errorf("could not record a session: %w", err)
	}

	return nil
}

// FindSession is the live session with this digest.
func (a *Accounts) FindSession(ctx context.Context, tokenHash string) (store.Session, bool, error) {
	var (
		session            store.Session
		created, expiresAt string
	)

	err := a.held.reads.QueryRowContext(ctx, `
		SELECT token_hash, actor, created_at, expires_at FROM sessions
		WHERE token_hash = ? AND revoked_at IS NULL AND expires_at > ?
	`, tokenHash, time.Now().UTC().Format(sortableNano)).Scan(&session.TokenHash, &session.Actor, &created, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Session{}, false, nil
	}

	if err != nil {
		return store.Session{}, false, fmt.Errorf("could not read a session: %w", err)
	}

	session.CreatedAt, session.ExpiresAt = parseTimestamp(created), parseTimestamp(expiresAt)

	return session, true, nil
}

// RevokeSession ends a session; revoking one that is already gone is not an error, since signing out twice signs out once.
func (a *Accounts) RevokeSession(ctx context.Context, tokenHash string) error {
	_, err := a.held.db.ExecContext(ctx, `UPDATE sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL`, nowNano(), tokenHash)
	if err != nil {
		return fmt.Errorf("could not revoke a session: %w", err)
	}

	return nil
}

// SaveUser replaces what was last recorded about a person.
func (a *Accounts) SaveUser(ctx context.Context, user store.User) error {
	orgs, groups := jsonStrings(user.Organizations), jsonStrings(user.Groups)

	_, err := a.held.db.ExecContext(ctx, `
		INSERT INTO users (actor, email, organizations, groups_json, refreshed_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (actor) DO UPDATE SET email = excluded.email, organizations = excluded.organizations,
			groups_json = excluded.groups_json, refreshed_at = excluded.refreshed_at
	`, user.Actor, user.Email, orgs, groups, user.RefreshedAt.UTC().Format(sortableNano))
	if err != nil {
		return fmt.Errorf("could not record user %q: %w", user.Actor, err)
	}

	return nil
}

// FindUser is the person an actor names.
func (a *Accounts) FindUser(ctx context.Context, who string) (store.User, bool, error) {
	var (
		user                      store.User
		orgs, groups, refreshedAt string
	)

	err := a.held.reads.QueryRowContext(ctx, `
		SELECT actor, email, organizations, groups_json, refreshed_at FROM users WHERE actor = ?
	`, who).Scan(&user.Actor, &user.Email, &orgs, &groups, &refreshedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.User{}, false, nil
	}

	if err != nil {
		return store.User{}, false, fmt.Errorf("could not read user %q: %w", who, err)
	}

	_ = json.Unmarshal([]byte(orgs), &user.Organizations)
	_ = json.Unmarshal([]byte(groups), &user.Groups)
	user.RefreshedAt = parseTimestamp(refreshedAt)

	return user, true, nil
}

// jsonStrings is a list as the JSON array a column keeps, never null.
func jsonStrings(list []string) string {
	if len(list) == 0 {
		return "[]"
	}

	encoded, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}

	return string(encoded)
}
