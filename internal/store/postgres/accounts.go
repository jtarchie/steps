package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// Accounts is the daemon-level handle on the people who sign in; see the sqlite driver.
type Accounts struct {
	held *pool
}

var _ store.Accounts = (*Accounts)(nil)

// OpenAccounts connects and makes sure the schema is there, the way OpenStore does for its first pipeline.
func OpenAccounts(rawURL string) (*Accounts, error) {
	held, err := acquirePool(rawURL)
	if err != nil {
		return nil, err
	}

	err = inSchema(context.Background(), held.connection, func(*sql.Tx) error { return nil })
	if err != nil {
		if held.release() {
			_ = held.db.Close()
		}

		return nil, err
	}

	return &Accounts{held: held}, nil
}

// Close lets go of the URL's pool, closing it if nothing else holds it.
func (a *Accounts) Close() error {
	if a.held.release() {
		return a.held.db.Close() //nolint:wrapcheck // closing a pool says nothing a caller acts on
	}

	return nil
}

// CreateSession records a signed-in browser and sweeps the ones that ended.
func (a *Accounts) CreateSession(ctx context.Context, session store.Session) error {
	err := inTx(ctx, a.held.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= $1 OR revoked_at IS NOT NULL`, now())
		if err != nil {
			return err //nolint:wrapcheck // wrapped below
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO sessions (token_hash, actor, created_at, expires_at) VALUES ($1, $2, $3, $4)
		`, clean(session.TokenHash), clean(session.Actor), session.CreatedAt.UTC(), session.ExpiresAt.UTC())

		return err //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return fmt.Errorf("could not record a session: %w", err)
	}

	return nil
}

// FindSession is the live session with this digest.
func (a *Accounts) FindSession(ctx context.Context, tokenHash string) (store.Session, bool, error) {
	var session store.Session

	err := a.held.db.QueryRowContext(ctx, `
		SELECT token_hash, actor, created_at, expires_at FROM sessions
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > $2
	`, tokenHash, now()).Scan(&session.TokenHash, &session.Actor, &session.CreatedAt, &session.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Session{}, false, nil
	}

	if err != nil {
		return store.Session{}, false, fmt.Errorf("could not read a session: %w", err)
	}

	session.CreatedAt, session.ExpiresAt = session.CreatedAt.UTC(), session.ExpiresAt.UTC()

	return session, true, nil
}

// RevokeSession ends a session; a second sign-out is not an error.
func (a *Accounts) RevokeSession(ctx context.Context, tokenHash string) error {
	_, err := a.held.db.ExecContext(ctx, `UPDATE sessions SET revoked_at = $1 WHERE token_hash = $2 AND revoked_at IS NULL`, now(), tokenHash)
	if err != nil {
		return fmt.Errorf("could not revoke a session: %w", err)
	}

	return nil
}
