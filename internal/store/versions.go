package store

import (
	"context"
	"time"
)

// Versions is resource version history and the three marks kept against it:
// what a check SAW, what a job PASSED, and how far a job has CONSUMED.
//
// One facet rather than two because the marks are meaningless without the
// history they point into — a passed row cascades off the version it names, so
// a cap on one is a cap on the other.
type Versions interface {
	// RecordVersions files what a check reported and prunes beyond limit,
	// returning how many were new to check-history. A version already filed
	// keeps its discovery order. Zero means no limit, as Retention's fields
	// do; a negative limit prunes at DefaultResourceVersionCap.
	RecordVersions(ctx context.Context, resourceName string, versions []map[string]any, limit int) (int, error)
	ResourceVersionsJSON(ctx context.Context, resourceName string) ([]string, error)
	VersionOrders(ctx context.Context, resourceName string) (map[string]int64, error)
	RecordVersionOrder(ctx context.Context, resourceName, versionJSON string) (int64, error)
	GreenVersions(ctx context.Context, resourceName string, upstreamJobs []string) ([]map[string]any, error)
	RecordCheckedVersion(ctx context.Context, resourceName, versionJSON string) error
	LastChecked(ctx context.Context, resourceName string) (CheckedResource, bool, error)
	CheckedResources(ctx context.Context) ([]CheckedResource, error)
	RecordPassedVersion(ctx context.Context, jobName, resourceName, versionJSON, buildID string) error
	PassedVersions(ctx context.Context, jobName string, limit int) ([]PassedVersion, error)
	HasPassedVersionSet(ctx context.Context, jobName string, want map[string]string) (bool, error)
	ConsumedMark(ctx context.Context, jobName, resourceName string) (int64, error)
	RecordConsumedMark(ctx context.Context, jobName, resourceName string, order int64) error
	RecordRunInput(ctx context.Context, runID, resourceName, versionJSON string) error
	RunInputs(ctx context.Context, runID string) (map[string]map[string]bool, error)
}

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
