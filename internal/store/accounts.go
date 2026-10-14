package store

import (
	"context"
	"errors"
	"time"
)

// Accounts is what a daemon keeps about the people who sign in to it, beside
// the pipelines rather than inside one: a session is somebody's standing with
// the daemon, and no pipeline handle is the right scope for it. One per
// database, opened by the driver on its own (sqlite.OpenAccounts,
// postgres.OpenAccounts), the way Identity is.
type Accounts interface {
	// CreateSession records a signed-in browser. The token itself is never
	// stored, only its digest, so a copy of the database signs nobody in.
	CreateSession(ctx context.Context, session Session) error
	// FindSession is the live session with this digest; found is false for
	// one that never existed, was revoked, or has expired.
	FindSession(ctx context.Context, tokenHash string) (Session, bool, error)
	// RevokeSession ends a session at once, which is what signing out is.
	RevokeSession(ctx context.Context, tokenHash string) error
	// SaveUser records what a sign-in provider said about a person, replacing
	// what it said last time: organizations and groups are read at sign-in,
	// so a person removed from a team loses it at their next.
	SaveUser(ctx context.Context, user User) error
	// FindUser is the person an actor names, as last recorded.
	FindUser(ctx context.Context, actor string) (User, bool, error)
	// RegisterClient records an agent that registered itself to connect to
	// exports (RFC 7591), refusing one past limit: registration answers
	// anybody, so its table must not grow without bound.
	RegisterClient(ctx context.Context, client OAuthClient, limit int) error
	FindClient(ctx context.Context, id string) (OAuthClient, bool, error)
	// IssueToken records a token by its digest, never the token.
	IssueToken(ctx context.Context, token OAuthToken) error
	// FindToken is the live token with this digest: unexpired, unrevoked.
	FindToken(ctx context.Context, tokenHash string) (OAuthToken, bool, error)
	// RevokeToken ends one token, which is what rotating a refresh token does to the old one.
	RevokeToken(ctx context.Context, tokenHash string) error
	// RevokeGrant ends every token a person gave one client for one export: disconnecting it.
	RevokeGrant(ctx context.Context, grant Grant) error
	Close() error
}

// Session is one signed-in browser.
type Session struct {
	TokenHash string
	// Actor is who it signed in as, actor.Actor's stored form.
	Actor     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// User is a person a sign-in provider vouched for, as it last described them.
type User struct {
	// Actor is who they are to every record, actor.Actor's stored form.
	Actor string
	// Email is the address the provider gave, empty when it gave none.
	Email string
	// Organizations and Groups are what permission rules match on: GitHub
	// organizations and "org/team" teams, a Google Workspace domain.
	Organizations []string
	Groups        []string
	RefreshedAt   time.Time
}

// ErrTooManyClients is a registration past the limit.
var ErrTooManyClients = errors.New("too many registered clients")

// OAuthClient is an agent that registered itself to connect to exports.
type OAuthClient struct {
	ID string
	// Name is what the client calls itself, which nothing verifies.
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time
}

// Grant is one person letting one client use one export.
type Grant struct {
	ClientID string
	Actor    string
	Pipeline string
	Export   string
}

// OAuthToken is a token issued under a grant.
type OAuthToken struct {
	TokenHash string
	// Kind is "access" or "refresh": an access token calls tools, a refresh token only mints the next access token.
	Kind      string
	Grant     Grant
	CreatedAt time.Time
	ExpiresAt time.Time
}
