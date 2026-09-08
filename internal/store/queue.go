package store

import (
	"time"
)

// QueueRow is one entry in the downstream-trigger queue.
type QueueRow struct {
	ID         int64
	JobName    string
	Reason     string
	Status     string
	EnqueuedAt time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	Error      string
}
