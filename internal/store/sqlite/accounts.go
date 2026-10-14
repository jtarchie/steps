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
		INSERT INTO users (actor, account_id, email, organizations, groups_json, refreshed_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (actor) DO UPDATE SET account_id = excluded.account_id, email = excluded.email, organizations = excluded.organizations,
			groups_json = excluded.groups_json, refreshed_at = excluded.refreshed_at
	`, user.Actor, user.AccountID, user.Email, orgs, groups, user.RefreshedAt.UTC().Format(sortableNano))
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
		SELECT actor, account_id, email, organizations, groups_json, refreshed_at FROM users WHERE actor = ?
	`, who).Scan(&user.Actor, &user.AccountID, &user.Email, &orgs, &groups, &refreshedAt)
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

// RegisterClient records a self-registered agent, refusing one past limit. The count and the insert share a transaction, so two registrations racing for the last place cannot both have it.
func (a *Accounts) RegisterClient(ctx context.Context, client store.OAuthClient, limit int) error {
	tx, err := a.held.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not register a client: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	var count int

	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_clients`).Scan(&count)
	if err != nil {
		return fmt.Errorf("could not register a client: %w", err)
	}

	if limit > 0 && count >= limit {
		return store.ErrTooManyClients
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO oauth_clients (id, name, redirect_uris, created_at) VALUES (?, ?, ?, ?)`,
		client.ID, client.Name, jsonStrings(client.RedirectURIs), client.CreatedAt.UTC().Format(sortableNano))
	if err != nil {
		return fmt.Errorf("could not register a client: %w", err)
	}

	return tx.Commit() //nolint:wrapcheck // a commit failure says nothing more with a prefix
}

// FindClient is a registered agent by its id.
func (a *Accounts) FindClient(ctx context.Context, id string) (store.OAuthClient, bool, error) {
	var (
		client             store.OAuthClient
		redirects, created string
	)

	err := a.held.reads.QueryRowContext(ctx, `SELECT id, name, redirect_uris, created_at FROM oauth_clients WHERE id = ?`, id).
		Scan(&client.ID, &client.Name, &redirects, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return store.OAuthClient{}, false, nil
	}

	if err != nil {
		return store.OAuthClient{}, false, fmt.Errorf("could not read a client: %w", err)
	}

	_ = json.Unmarshal([]byte(redirects), &client.RedirectURIs)
	client.CreatedAt = parseTimestamp(created)

	return client, true, nil
}

// IssueToken records a token by its digest, sweeping ones that ended.
func (a *Accounts) IssueToken(ctx context.Context, token store.OAuthToken) error {
	_, err := a.held.db.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE expires_at <= ? OR revoked_at IS NOT NULL`, nowNano())
	if err != nil {
		return fmt.Errorf("could not sweep ended tokens: %w", err)
	}

	_, err = a.held.db.ExecContext(ctx, `
		INSERT INTO oauth_tokens (token_hash, kind, client_id, actor, pipeline, export, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, token.TokenHash, token.Kind, token.Grant.ClientID, token.Grant.Actor, token.Grant.Pipeline, token.Grant.Export,
		token.CreatedAt.UTC().Format(sortableNano), token.ExpiresAt.UTC().Format(sortableNano))
	if err != nil {
		return fmt.Errorf("could not issue a token: %w", err)
	}

	return nil
}

// FindToken is the live token with this digest.
func (a *Accounts) FindToken(ctx context.Context, tokenHash string) (store.OAuthToken, bool, error) {
	var (
		token              store.OAuthToken
		created, expiresAt string
	)

	err := a.held.reads.QueryRowContext(ctx, `
		SELECT token_hash, kind, client_id, actor, pipeline, export, created_at, expires_at FROM oauth_tokens
		WHERE token_hash = ? AND revoked_at IS NULL AND expires_at > ?
	`, tokenHash, nowNano()).Scan(&token.TokenHash, &token.Kind, &token.Grant.ClientID, &token.Grant.Actor,
		&token.Grant.Pipeline, &token.Grant.Export, &created, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.OAuthToken{}, false, nil
	}

	if err != nil {
		return store.OAuthToken{}, false, fmt.Errorf("could not read a token: %w", err)
	}

	token.CreatedAt, token.ExpiresAt = parseTimestamp(created), parseTimestamp(expiresAt)

	return token, true, nil
}

// RevokeToken ends one token.
func (a *Accounts) RevokeToken(ctx context.Context, tokenHash string) (bool, error) {
	result, err := a.held.db.ExecContext(ctx, `UPDATE oauth_tokens SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL`, nowNano(), tokenHash)
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
		UPDATE oauth_tokens SET revoked_at = ?
		WHERE client_id = ? AND actor = ? AND pipeline = ? AND export = ? AND revoked_at IS NULL
	`, nowNano(), grant.ClientID, grant.Actor, grant.Pipeline, grant.Export)
	if err != nil {
		return fmt.Errorf("could not revoke a grant: %w", err)
	}

	return nil
}

// Grants are the live connections, newest first.
func (a *Accounts) Grants(ctx context.Context, filter store.Grant) ([]store.Connection, error) {
	rows, err := a.held.reads.QueryContext(ctx, `
		SELECT t.client_id, c.name, c.redirect_uris, t.actor, t.pipeline, t.export, MIN(t.created_at)
		FROM oauth_tokens t JOIN oauth_clients c ON c.id = t.client_id
		WHERE t.revoked_at IS NULL AND t.expires_at > ?
		  AND (? = '' OR t.client_id = ?) AND (? = '' OR t.actor = ?)
		  AND (? = '' OR t.pipeline = ?) AND (? = '' OR t.export = ?)
		GROUP BY t.client_id, c.name, c.redirect_uris, t.actor, t.pipeline, t.export
		ORDER BY MIN(t.created_at) DESC
	`, nowNano(), filter.ClientID, filter.ClientID, filter.Actor, filter.Actor, filter.Pipeline, filter.Pipeline, filter.Export, filter.Export)
	if err != nil {
		return nil, fmt.Errorf("could not read connections: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var connections []store.Connection

	for rows.Next() {
		var (
			conn               store.Connection
			redirects, created string
		)

		err = rows.Scan(&conn.ClientID, &conn.Client, &redirects, &conn.Actor, &conn.Pipeline, &conn.Export, &created)
		if err != nil {
			return nil, fmt.Errorf("could not read connections: %w", err)
		}

		_ = json.Unmarshal([]byte(redirects), &conn.RedirectURIs)
		conn.AuthorizedAt = parseTimestamp(created)
		connections = append(connections, conn)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read connections: %w", err)
	}

	return connections, nil
}
