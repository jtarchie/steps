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
