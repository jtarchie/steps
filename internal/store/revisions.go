package store

import (
	"context"
)

// Revisions is the configuration a run was executed under, interned once by
// its hash.
type Revisions interface {
	RecordRevision(ctx context.Context, sha, source string) error
	FindRevision(ctx context.Context, sha string) (Revision, bool, error)
}

// Revision is one recorded configuration: the substituted source a run was
// started from, and the hash that identifies it.
type Revision struct {
	SHA    string
	Source string
}
