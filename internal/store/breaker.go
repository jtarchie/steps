package store

// PausedJob is one job the breaker has stopped, with the count that stopped it.
type PausedJob struct {
	Name        string
	Consecutive int
	PausedAt    string
}
