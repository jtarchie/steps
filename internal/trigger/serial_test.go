package trigger

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// TestSerialGroupsBlockConcurrentClaims is the hazard the feature exists for:
// two jobs that mutate the same deploy target must never be in flight at once,
// however many workers `steps web --max-concurrent` is running.
func TestSerialGroupsBlockConcurrentClaims(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, t.TempDir())
	ctx := context.Background()

	cfg := &config.Config{Jobs: []config.Job{
		{Name: "deploy-staging", SerialGroups: []string{"deploy-lock"}},
		{Name: "deploy-prod", SerialGroups: []string{"deploy-lock"}},
		{Name: "unrelated"},
	}}

	err := st.SyncJobLimits(ctx, cfg.SerialGroupsByJob(), nil)
	if err != nil {
		t.Fatalf("SyncJobLimits: %v", err)
	}

	for _, name := range []string{"deploy-staging", "deploy-prod", "unrelated"} {
		enqueueErr := st.EnqueueJob(ctx, name, "a new version")
		if enqueueErr != nil {
			t.Fatalf("EnqueueJob(%s): %v", name, enqueueErr)
		}
	}

	// First claim takes the lock.
	if got := mustClaim(t, st); got != "deploy-staging" {
		t.Fatalf("first claim = %q, want the oldest pending row", got)
	}

	// The next claim must skip its group-mate and take the unrelated job.
	if got := mustClaim(t, st); got != "unrelated" {
		t.Fatalf("second claim = %q, want the job outside the group (deploy-prod shares the held lock)", got)
	}

	// And nothing else is claimable while the lock is held.
	if got := mustClaim(t, st); got != "" {
		t.Fatalf("claimed %q while the deploy lock was held", got)
	}
}

// mustClaim claims the next job, returning "" when nothing is claimable.
func mustClaim(t *testing.T, st store.Store) string {
	t.Helper()

	_, name, found, err := st.ClaimNextJob(context.Background())
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}

	if !found {
		return ""
	}

	return name
}

// TestSerialGroupHolderNamesWhoHasIt covers the reporting half. "Queued" and
// "blocked on a lock" look identical from the outside — nothing running,
// nothing said — and an operator who cannot tell them apart cannot tell a
// stuck pipeline from a busy one.
func TestSerialGroupHolderNamesWhoHasIt(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, t.TempDir())
	ctx := context.Background()

	cfg := &config.Config{Jobs: []config.Job{
		{Name: "deploy-staging", SerialGroups: []string{"deploy-lock"}},
		{Name: "deploy-prod", SerialGroups: []string{"deploy-lock"}},
	}}

	err := st.SyncJobLimits(ctx, cfg.SerialGroupsByJob(), nil)
	if err != nil {
		t.Fatalf("SyncJobLimits: %v", err)
	}

	holder, err := st.SerialGroupHolder(ctx, "deploy-prod")
	if err != nil || holder != "" {
		t.Fatalf("holder = %q, %v; want nothing holding the lock yet", holder, err)
	}

	err = st.EnqueueJob(ctx, "deploy-staging", "a new version")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	_, _, _, err = st.ClaimNextJob(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	holder, err = st.SerialGroupHolder(ctx, "deploy-prod")
	if err != nil {
		t.Fatalf("SerialGroupHolder: %v", err)
	}

	if holder != "deploy-staging" {
		t.Errorf("holder = %q, want deploy-staging", holder)
	}
}

// serial_groups: without serial: true is still a lock, and a job held by it must say who holds it.
func TestReportSerialWaitsNamesAGroupHolder(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, t.TempDir())
	ctx := context.Background()

	cfg := &config.Config{Jobs: []config.Job{
		{Name: "deploy-staging", SerialGroups: []string{"deploy-lock"}},
		{Name: "deploy-prod", SerialGroups: []string{"deploy-lock"}},
	}}

	err := st.SyncJobLimits(ctx, cfg.SerialGroupsByJob(), nil)
	if err != nil {
		t.Fatalf("SyncJobLimits: %v", err)
	}

	err = st.EnqueueJob(ctx, "deploy-staging", "a new version")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	_, _, _, err = st.ClaimNextJob(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	printed := captureStdout(t, func() { reportSerialWaits(ctx, cfg, st) })
	if !strings.Contains(printed, "trigger: deploy-prod waiting: lock held by deploy-staging\n") {
		t.Errorf("printed %q, want deploy-prod told who holds its group's lock", printed)
	}
}

// TestSyncJobLimitsReplacesStaleMembership verifies a group removed from
// the pipeline stops holding a lock. A stale row would keep two jobs apart
// forever with nothing in the YAML to explain why.
func TestSyncJobLimitsReplacesStaleMembership(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, t.TempDir())
	ctx := context.Background()

	withGroups := &config.Config{Jobs: []config.Job{
		{Name: "a", SerialGroups: []string{"lock"}},
		{Name: "b", SerialGroups: []string{"lock"}},
	}}

	err := st.SyncJobLimits(ctx, withGroups.SerialGroupsByJob(), nil)
	if err != nil {
		t.Fatalf("SyncJobLimits: %v", err)
	}

	// The pipeline drops the groups.
	err = st.SyncJobLimits(ctx, (&config.Config{Jobs: []config.Job{{Name: "a"}, {Name: "b"}}}).SerialGroupsByJob(), nil)
	if err != nil {
		t.Fatalf("SyncJobLimits (cleared): %v", err)
	}

	for _, name := range []string{"a", "b"} {
		enqueueErr := st.EnqueueJob(ctx, name, "a new version")
		if enqueueErr != nil {
			t.Fatalf("EnqueueJob(%s): %v", name, enqueueErr)
		}
	}

	if got := mustClaim(t, st); got == "" {
		t.Fatal("nothing was claimable at all")
	}

	if got := mustClaim(t, st); got == "" {
		t.Error("a removed serial group is still holding a lock")
	}
}
