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
	// CompareAndSetCheckedVersion moves the checked version to next only if it is still expected — or, with expectedFound false, still absent — reporting whether it moved. It is how a poll advances a resource something else may have advanced since the poll read it.
	CompareAndSetCheckedVersion(ctx context.Context, resourceName, expected string, expectedFound bool, next string) (bool, error)
	LastChecked(ctx context.Context, resourceName string) (CheckedResource, bool, error)
	CheckedResources(ctx context.Context) ([]CheckedResource, error)
	// RecordCheckError files why a resource's check failed; an empty message
	// clears what was filed. A check that errors leaves the recorded version
	// exactly where it was — which is correct, and which is why a failing
	// resource is otherwise indistinguishable from a quiet one.
	RecordCheckError(ctx context.Context, resourceName, message string) error
	// CheckErrors is every resource whose last check failed, by name. It is
	// kept apart from CheckedResources because the two answer different
	// questions about different rows: a resource that has never checked
	// successfully has an error and no version at all.
	CheckErrors(ctx context.Context) ([]CheckError, error)
	RecordPassedVersion(ctx context.Context, jobName, resourceName, versionJSON, buildID string) error
	PassedVersions(ctx context.Context, jobName string, limit int) ([]PassedVersion, error)
	HasPassedVersionSet(ctx context.Context, jobName string, want map[string]string) (bool, error)
	ConsumedMark(ctx context.Context, jobName, resourceName string) (int64, error)
	RecordConsumedMark(ctx context.Context, jobName, resourceName string, order int64) error
	RecordRunInput(ctx context.Context, runID, buildID, resourceName, versionJSON string) error
	RunInputs(ctx context.Context, runID string) ([]RunInput, error)
}

// RunInput is a version one build of a run was created with.
type RunInput struct {
	BuildID  string
	Resource string
	Version  string
}

// CheckedResource is a resource's last observed version.
type CheckedResource struct {
	Name      string
	Version   string
	CheckedAt time.Time
}

// CheckError is a resource whose last check failed, and what it said.
//
// Recorded because a failing check is otherwise SILENT: the poll aborts on
// the first resource that errors, the recorded version stays where it was,
// and the only trace is a log line on the machine running the daemon.
type CheckError struct {
	Name     string
	Message  string
	FailedAt time.Time
}

// PassedVersion is one resource version a job succeeded against.
type PassedVersion struct {
	Resource   string
	Version    string
	RecordedAt time.Time
}
