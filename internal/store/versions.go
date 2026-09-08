package store

import (
	"time"
)

// CheckedResource is a resource's last observed version.
type CheckedResource struct {
	Name      string
	Version   string
	CheckedAt time.Time
}

// PassedVersion is one resource version a job succeeded against.
type PassedVersion struct {
	Resource   string
	Version    string
	RecordedAt time.Time
}
