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
	// anybody, so its table must not grow without bound. Before counting it
	// evicts clients holding no token that are older than UnusedClientTTL.
	RegisterClient(ctx context.Context, client OAuthClient, limit int) error
	FindClient(ctx context.Context, id string) (OAuthClient, bool, error)
	// IssueToken records a token by its digest, never the token.
	IssueToken(ctx context.Context, token OAuthToken) error
	// FindToken is the live token with this digest: unexpired, unrevoked.
	FindToken(ctx context.Context, tokenHash string) (OAuthToken, bool, error)
	// RevokeToken ends one token, which is what rotating a refresh token does
	// to the old one. revoked is false for a token already ended: of two
	// refreshes racing with one token, only the one that ended it may mint.
	RevokeToken(ctx context.Context, tokenHash string) (revoked bool, err error)
	// RevokeGrant ends every token a person gave one client under one grant, kind included: disconnecting it.
	RevokeGrant(ctx context.Context, grant Grant) error
	// Grants are the live connections — a grant with a token still good —
	// newest first, narrowed by filter's non-empty fields.
	Grants(ctx context.Context, filter Grant) ([]Connection, error)
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
	// AccountID is the provider's own id for the account, which survives a rename where the name in Actor does not: GitHub's numeric user id, Google's subject.
	AccountID string
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

// UnusedClientTTL is how long a registered client with no token may sit before a registration is free to evict it; it is the only thing that frees a place, since registration answers anybody and nothing else deletes a client.
const UnusedClientTTL = 24 * time.Hour

// OAuthClient is an agent that registered itself to connect to exports.
type OAuthClient struct {
	ID string
	// Name is what the client calls itself, which nothing verifies.
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time
}

// Grant is one person letting one client in: to one export, or to read what they can see.
type Grant struct {
	// Kind says which, explicitly: a read grant names no pipeline and no export, and an empty export must never come to mean "everything".
	Kind     GrantKind
	ClientID string
	Actor    string
	Pipeline string
	Export   string
}

// GrantKind is what a grant lets its client do, and so the one address its tokens are good at.
type GrantKind string

// The kinds a grant is stored with.
const (
	// GrantExport calls one pipeline's export, at /mcp/x/<pipeline>/<export>.
	GrantExport GrantKind = "export"
	// GrantRead reads every pipeline its person can see, at /mcp.
	GrantRead GrantKind = "read"
)

// ErrUnknownGrantKind is a token issued under a grant that says neither kind.
var ErrUnknownGrantKind = errors.New("a grant is for an export or for reading, and this one says neither")

// Valid is whether k is one of the kinds a token may be issued under.
func (k GrantKind) Valid() bool { return k == GrantExport || k == GrantRead }

// TokenKind says what an OAuth token may do: an access token calls tools, a refresh token only mints the next access token, and a display token only shows /wall.
type TokenKind string

// The kinds a token is stored with.
const (
	TokenAccess  TokenKind = "access"
	TokenRefresh TokenKind = "refresh"
	TokenDisplay TokenKind = "display"
)

// OAuthToken is a token issued under a grant.
type OAuthToken struct {
	TokenHash string
	Kind      TokenKind
	Grant     Grant
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Connection is a live grant and the client it was given to.
type Connection struct {
	Grant
	// Client is what the agent called itself, and RedirectURIs where it said to send people back.
	Client       string
	RedirectURIs []string
	// AuthorizedAt is when the person first agreed, the oldest of the grant's live tokens.
	AuthorizedAt time.Time
}
