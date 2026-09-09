package store

import (
	"context"
)

// Control is the pipeline-level switchboard a `steps pipeline` verb throws: whether it admits work, what it is called, and whether it exists at all.
type Control interface {
	// Pause is the circuit breaker for the whole pipeline: nothing polls, nothing is admitted, until Unpause.
	Pause(ctx context.Context) error
	Unpause(ctx context.Context) error
	Paused(ctx context.Context) (bool, error)
	// Rename is one UPDATE and keeps history; the handle goes on answering under its old name, so a caller closes it and re-opens under the new one.
	Rename(ctx context.Context, name string) error
	// Delete forgets the pipeline and everything that cascades off it; the handle is spent afterwards and must be closed.
	Delete(ctx context.Context) error
}
