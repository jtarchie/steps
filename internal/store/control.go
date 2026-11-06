package store

import (
	"context"
)

// Control is the pipeline-level switchboard a `steps pipeline` verb throws: whether it admits work, what it is called, where its YAML lives, and whether it exists at all.
type Control interface {
	// Pause is the circuit breaker for the whole pipeline: nothing polls, nothing is admitted, until Unpause.
	Pause(ctx context.Context) error
	Unpause(ctx context.Context) error
	// Paused is whether the pipeline admits nothing: its own Pause, or PauseAll. Every gate asks this one, so a new gate cannot forget the daemon-wide pause.
	Paused(ctx context.Context) (bool, error)
	// PipelinePaused is its own Pause alone, what Unpause clears and what it returns to once UnpauseAll lifts the daemon-wide one.
	PipelinePaused(ctx context.Context) (bool, error)
	// PauseAll pauses every pipeline in the database without touching any one's own pause, so UnpauseAll hands each back the state it had: a stop gap, not a bulk Pause.
	PauseAll(ctx context.Context) error
	UnpauseAll(ctx context.Context) error
	AllPaused(ctx context.Context) (bool, error)
	// Rename is one UPDATE and keeps history; the handle goes on answering under its old name, so a caller closes it and re-opens under the new one.
	Rename(ctx context.Context, name string) error
	// Delete forgets the pipeline and everything that cascades off it; the handle is spent afterwards and must be closed.
	Delete(ctx context.Context) error
	// SetSourcePath records where this pipeline's YAML lives, so a reader of
	// the database can tell two checkouts apart. Only a command that LOADED
	// the pipeline calls it.
	SetSourcePath(ctx context.Context, source string) error
}
