package store

import "time"

// HeldJob is one job the breaker has stopped, with the count that stopped it.
type HeldJob struct {
	Name        string
	Consecutive int
	HeldAt      time.Time
}
