package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/jtarchie/steps/internal/store"
)

// Accounts is the daemon-level handle on the people who sign in; see the sqlite driver.
type Accounts struct {
	held     *pool
	released atomic.Bool
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
	if a.released.Swap(true) {
		return nil
	}

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

// SaveUser replaces what was last recorded about a person.
func (a *Accounts) SaveUser(ctx context.Context, user store.User) error {
	orgs, groups := jsonStrings(user.Organizations), jsonStrings(user.Groups)

	_, err := a.held.db.ExecContext(ctx, `
		INSERT INTO users (actor, account_id, email, organizations, groups_json, refreshed_at) VALUES ($1, $6, $2, $3, $4, $5)
		ON CONFLICT (actor) DO UPDATE SET account_id = excluded.account_id, email = excluded.email, organizations = excluded.organizations,
			groups_json = excluded.groups_json, refreshed_at = excluded.refreshed_at
	`, clean(user.Actor), clean(user.Email), orgs, groups, user.RefreshedAt.UTC(), clean(user.AccountID))
	if err != nil {
		return fmt.Errorf("could not record user %q: %w", user.Actor, err)
	}

	return nil
}

// FindUser is the person an actor names.
func (a *Accounts) FindUser(ctx context.Context, who string) (store.User, bool, error) {
	var (
		user         store.User
		orgs, groups string
	)

	err := a.held.db.QueryRowContext(ctx, `
		SELECT actor, account_id, email, organizations, groups_json, refreshed_at FROM users WHERE actor = $1
	`, who).Scan(&user.Actor, &user.AccountID, &user.Email, &orgs, &groups, &user.RefreshedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.User{}, false, nil
	}

	if err != nil {
		return store.User{}, false, fmt.Errorf("could not read user %q: %w", who, err)
	}

	// A corrupt column must not read as "no groups": a rule written as an exclusion would then match.
	err = errors.Join(json.Unmarshal([]byte(orgs), &user.Organizations), json.Unmarshal([]byte(groups), &user.Groups))
	if err != nil {
		return store.User{}, false, fmt.Errorf("could not decode user %q: %w", who, err)
	}

	user.RefreshedAt = user.RefreshedAt.UTC()

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

	return clean(string(encoded))
}

// RegisterClient records a self-registered agent, refusing one past limit; the count and the insert share a transaction.
func (a *Accounts) RegisterClient(ctx context.Context, client store.OAuthClient, limit int) error {
	err := inTx(ctx, a.held.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `LOCK TABLE oauth_clients IN SHARE ROW EXCLUSIVE MODE`)
		if err != nil {
			return err //nolint:wrapcheck // wrapped below
		}

		_, err = tx.ExecContext(ctx, `DELETE FROM oauth_clients WHERE created_at < $1 AND id NOT IN (SELECT client_id FROM oauth_tokens)`,
			client.CreatedAt.Add(-store.UnusedClientTTL).UTC())
		if err != nil {
			return err //nolint:wrapcheck // wrapped below
		}

		var count int

		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_clients`).Scan(&count)
		if err != nil {
			return err //nolint:wrapcheck // wrapped below
		}

		if limit > 0 && count >= limit {
			return store.ErrTooManyClients
		}

		_, err = tx.ExecContext(ctx, `INSERT INTO oauth_clients (id, name, redirect_uris, created_at) VALUES ($1, $2, $3, $4)`,
			clean(client.ID), clean(client.Name), jsonStrings(client.RedirectURIs), client.CreatedAt.UTC())

		return err //nolint:wrapcheck // wrapped below
	})
	if errors.Is(err, store.ErrTooManyClients) {
		return err
	}

	if err != nil {
		return fmt.Errorf("could not register a client: %w", err)
	}

	return nil
}

// FindClient is a registered agent by its id.
func (a *Accounts) FindClient(ctx context.Context, id string) (store.OAuthClient, bool, error) {
	var (
		client    store.OAuthClient
		redirects string
	)

	err := a.held.db.QueryRowContext(ctx, `SELECT id, name, redirect_uris, created_at FROM oauth_clients WHERE id = $1`, id).
		Scan(&client.ID, &client.Name, &redirects, &client.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.OAuthClient{}, false, nil
	}

	if err != nil {
		return store.OAuthClient{}, false, fmt.Errorf("could not read a client: %w", err)
	}

	_ = json.Unmarshal([]byte(redirects), &client.RedirectURIs)
	client.CreatedAt = client.CreatedAt.UTC()

	return client, true, nil
}

// IssueToken records a token by its digest, sweeping ones that ended.
func (a *Accounts) IssueToken(ctx context.Context, token store.OAuthToken) error {
	err := inTx(ctx, a.held.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE expires_at <= $1 OR revoked_at IS NOT NULL`, now())
		if err != nil {
			return err //nolint:wrapcheck // wrapped below
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO oauth_tokens (token_hash, kind, client_id, actor, pipeline, export, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, clean(token.TokenHash), clean(token.Kind), clean(token.Grant.ClientID), clean(token.Grant.Actor),
			clean(token.Grant.Pipeline), clean(token.Grant.Export), token.CreatedAt.UTC(), token.ExpiresAt.UTC())

		return err //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return fmt.Errorf("could not issue a token: %w", err)
	}

	return nil
}

// FindToken is the live token with this digest.
func (a *Accounts) FindToken(ctx context.Context, tokenHash string) (store.OAuthToken, bool, error) {
	var token store.OAuthToken

	err := a.held.db.QueryRowContext(ctx, `
		SELECT token_hash, kind, client_id, actor, pipeline, export, created_at, expires_at FROM oauth_tokens
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > $2
	`, tokenHash, now()).Scan(&token.TokenHash, &token.Kind, &token.Grant.ClientID, &token.Grant.Actor,
		&token.Grant.Pipeline, &token.Grant.Export, &token.CreatedAt, &token.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.OAuthToken{}, false, nil
	}

	if err != nil {
		return store.OAuthToken{}, false, fmt.Errorf("could not read a token: %w", err)
	}

	token.CreatedAt, token.ExpiresAt = token.CreatedAt.UTC(), token.ExpiresAt.UTC()

	return token, true, nil
}

// RevokeToken ends one token.
func (a *Accounts) RevokeToken(ctx context.Context, tokenHash string) (bool, error) {
	result, err := a.held.db.ExecContext(ctx, `UPDATE oauth_tokens SET revoked_at = $1 WHERE token_hash = $2 AND revoked_at IS NULL`, now(), tokenHash)
	if err != nil {
		return false, fmt.Errorf("could not revoke a token: %w", err)
	}

	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("could not revoke a token: %w", err)
	}

	return changed > 0, nil
}

// RevokeGrant ends every token under one grant.
func (a *Accounts) RevokeGrant(ctx context.Context, grant store.Grant) error {
	_, err := a.held.db.ExecContext(ctx, `
		UPDATE oauth_tokens SET revoked_at = $1
		WHERE client_id = $2 AND actor = $3 AND pipeline = $4 AND export = $5 AND revoked_at IS NULL
	`, now(), grant.ClientID, grant.Actor, grant.Pipeline, grant.Export)
	if err != nil {
		return fmt.Errorf("could not revoke a grant: %w", err)
	}

	return nil
}

// Grants are the live connections, newest first.
func (a *Accounts) Grants(ctx context.Context, filter store.Grant) ([]store.Connection, error) {
	return collect(ctx, a.held.db, "connections", `
		SELECT t.client_id, c.name, c.redirect_uris, t.actor, t.pipeline, t.export, MIN(t.created_at)
		FROM oauth_tokens t JOIN oauth_clients c ON c.id = t.client_id
		WHERE t.revoked_at IS NULL AND t.expires_at > $1
		  AND ($2 = '' OR t.client_id = $2) AND ($3 = '' OR t.actor = $3)
		  AND ($4 = '' OR t.pipeline = $4) AND ($5 = '' OR t.export = $5)
		GROUP BY t.client_id, c.name, c.redirect_uris, t.actor, t.pipeline, t.export
		ORDER BY MIN(t.created_at) DESC
	`, []any{now(), filter.ClientID, filter.Actor, filter.Pipeline, filter.Export}, func(rows *sql.Rows) (store.Connection, error) {
		var (
			conn      store.Connection
			redirects string
		)

		err := rows.Scan(&conn.ClientID, &conn.Client, &redirects, &conn.Actor, &conn.Pipeline, &conn.Export, &conn.AuthorizedAt)
		_ = json.Unmarshal([]byte(redirects), &conn.RedirectURIs)
		conn.AuthorizedAt = conn.AuthorizedAt.UTC()

		return conn, err //nolint:wrapcheck // collect names what it was reading
	})
}
