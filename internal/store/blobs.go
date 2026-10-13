package store

import "context"

// Blobs is the step cache's durable half: what a cached step's outputs were,
// by action key, so a skip can restore them instead of re-earning them.
//
// The index is truth and the bytes live elsewhere — internal/workspace
// declares this same pair as StepIndex, because its depguard allow-list names
// neither this package nor a blob store, so both halves reach it as interfaces
// it declares and internal/cli wires together.
type Blobs interface {
	RecordStepBlobs(ctx context.Context, actionKey string, outputs map[string]string) error
	StepBlobs(ctx context.Context, actionKey string) (map[string]string, error)
}

// StepBlobEntryCap bounds how many step entries this index keeps per
// pipeline, newest INSERTED first — which is not the local cache's LRU: a
// local hit refreshes the entry's mtime but records nothing here, so under
// churn past the cap the rows evicted first are the hottest keys. Known and
// accepted: re-recording on every hit would cost a digest walk per hit, the
// loss is cost-only (the other machine re-runs and re-records), and the cap
// exists for bound, not fidelity. It mirrors the on-disk step cache's own entry bound
// (workspace's defaultStepCacheMaxEntries, 200): an index entry whose local
// bytes could never still be cached buys little, since the action keys of
// evicted entries stop being computed long before they stop being stored.
// Count, never age — the standing rule for caches in this schema.
const StepBlobEntryCap = 200
