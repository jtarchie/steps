package store

import (
	"context"
)

// Revisions is the configuration a run was executed under, interned once by its hash, and which of them a daemon currently serves.
type Revisions interface {
	// RecordRevision interns a configuration with the include contents it folded in; the same sha twice is one row.
	RecordRevision(ctx context.Context, sha, source string, includes map[string]string) error
	FindRevision(ctx context.Context, sha string) (Revision, bool, error)
	// SetCurrentRevision names the recorded revision a `steps pipeline set` made current and, in the same write, records from as the pipeline's source path, so the two cannot disagree; the current one is never reaped.
	SetCurrentRevision(ctx context.Context, sha, from string) error
	// CurrentRevision is what a daemon serves for this pipeline at startup; false for a pipeline that only ever ran by hand.
	CurrentRevision(ctx context.Context) (Revision, bool, error)
}

// Revision is one recorded configuration: the substituted source a run was started from, the includes it carried, and the hash that identifies both.
type Revision struct {
	SHA      string
	Source   string
	Includes map[string]string
}
