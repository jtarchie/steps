package store

import (
	"context"
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
