package trigger

import (
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// TestSerialGroupsBlockConcurrentClaims is the hazard the feature exists for:
// two jobs that mutate the same deploy target must never be in flight at once,
// however many workers `steps watch --max-concurrent` is running.
func TestSerialGroupsBlockConcurrentClaims(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, t.TempDir())
	ctx := context.Background()

	cfg := &config.Config{Jobs: []config.Job{
		{Name: "deploy-staging", SerialGroups: []string{"deploy-lock"}},
		{Name: "deploy-prod", SerialGroups: []string{"deploy-lock"}},
		{Name: "unrelated"},
	}}

	err := st.SyncSerialGroups(ctx, cfg.SerialGroupsByJob())
	if err != nil {
		t.Fatalf("SyncSerialGroups: %v", err)
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
func mustClaim(t *testing.T, st *store.Store) string {
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

	err := st.SyncSerialGroups(ctx, cfg.SerialGroupsByJob())
	if err != nil {
		t.Fatalf("SyncSerialGroups: %v", err)
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

// TestSyncSerialGroupsReplacesStaleMembership verifies a group removed from
// the pipeline stops holding a lock. A stale row would keep two jobs apart
// forever with nothing in the YAML to explain why.
func TestSyncSerialGroupsReplacesStaleMembership(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, t.TempDir())
	ctx := context.Background()

	withGroups := &config.Config{Jobs: []config.Job{
		{Name: "a", SerialGroups: []string{"lock"}},
		{Name: "b", SerialGroups: []string{"lock"}},
	}}

	err := st.SyncSerialGroups(ctx, withGroups.SerialGroupsByJob())
	if err != nil {
		t.Fatalf("SyncSerialGroups: %v", err)
	}

	// The pipeline drops the groups.
	err = st.SyncSerialGroups(ctx, (&config.Config{Jobs: []config.Job{{Name: "a"}, {Name: "b"}}}).SerialGroupsByJob())
	if err != nil {
		t.Fatalf("SyncSerialGroups (cleared): %v", err)
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
