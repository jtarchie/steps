package storetest

// Methods of the contract the suite had no test for until a second driver ran
// it and a coverage report showed them never executed there. Each was tested
// only through the sqlite driver, from a package above it — which proves
// nothing about another driver.

import (
	"context"
	"errors"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestTheBreakerCountsPausesAndResets: consecutive failures up to the limit
// pause a job, a success releases it, and no limit never pauses.
func (s suite) TestTheBreakerCountsPausesAndResets(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	assertOutcome(t, st, "deploy", false, 2, false, 1)
	assertOutcome(t, st, "deploy", false, 2, true, 2)

	held, err := st.PausedJobs(ctx)
	if err != nil || len(held) != 1 || held[0].Name != "deploy" || held[0].Consecutive != 2 || held[0].PausedAt == "" {
		t.Fatalf("PausedJobs = %+v, %v; want deploy held after 2 failures, with when", held, err)
	}

	assertOutcome(t, st, "deploy", true, 2, false, 0)
	assertNotPaused(t, st, "deploy")

	for failures := range 5 {
		assertOutcome(t, st, "unbounded", false, 0, false, failures+1)
	}

	assertNotPaused(t, st, "unbounded")
}

func assertOutcome(t *testing.T, st store.Store, job string, succeeded bool, maxFailures int, wantPaused bool, wantConsecutive int) {
	t.Helper()

	paused, consecutive, err := st.RecordJobOutcome(context.Background(), job, succeeded, maxFailures)
	if err != nil || paused != wantPaused || consecutive != wantConsecutive {
		t.Fatalf("RecordJobOutcome(%q, succeeded=%v) = paused %v, %d in a row, %v; want paused %v, %d in a row",
			job, succeeded, paused, consecutive, err, wantPaused, wantConsecutive)
	}
}

func assertNotPaused(t *testing.T, st store.Store, job string) {
	t.Helper()

	paused, err := st.IsJobPaused(context.Background(), job)
	if err != nil || paused {
		t.Fatalf("IsJobPaused(%q) = %v, %v; want false", job, paused, err)
	}
}

// TestSerialGroupHolderNamesWhoIsRunning: a job waiting on a lock can say
// whom it is waiting for, and nobody is named once the lock is free.
func (s suite) TestSerialGroupHolderNamesWhoIsRunning(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	err := st.SyncJobLimits(ctx, map[string][]string{"deploy-a": {"target"}, "deploy-b": {"target"}}, nil)
	if err != nil {
		t.Fatalf("SyncJobLimits: %v", err)
	}

	mustEnqueueJob(t, st, "deploy-a", "r")
	id, _ := mustClaimJob(t, st, "deploy-a")

	for job, want := range map[string]string{"deploy-b": "deploy-a", "unrelated": ""} {
		holder, err := st.SerialGroupHolder(ctx, job)
		if err != nil || holder != want {
			t.Errorf("SerialGroupHolder(%q) = %q, %v; want %q", job, holder, err, want)
		}
	}

	err = st.CompleteJob(ctx, id, "succeeded", nil)
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	holder, err := st.SerialGroupHolder(ctx, "deploy-b")
	if err != nil || holder != "" {
		t.Errorf("SerialGroupHolder after the holder finished = %q, %v; want nobody", holder, err)
	}
}

// TestHasNodeSucceededIsPerJobAndStatus: an across: cell asks about itself,
// and only a green node of its own job answers yes.
func (s suite) TestHasNodeSucceededIsPerJobAndStatus(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	mustRecordNode(t, st, "build", hashOf(1))

	err := st.RecordNode(ctx, store.NodeRecord{Hash: hashOf(2), Kind: "task", Content: map[string]any{"n": 2}},
		"build", "failed", nil, errors.New("boom"))
	if err != nil {
		t.Fatalf("RecordNode: %v", err)
	}

	for _, probe := range []struct {
		job, hash string
		want      bool
	}{
		{"build", hashOf(1), true},
		{"other", hashOf(1), false},
		{"build", hashOf(2), false},
		{"build", hashOf(3), false},
	} {
		got, err := st.HasNodeSucceeded(ctx, probe.job, probe.hash)
		if err != nil || got != probe.want {
			t.Errorf("HasNodeSucceeded(%q, …%s) = %v, %v; want %v", probe.job, probe.hash[60:], got, err, probe.want)
		}
	}
}

// TestSourcePathIsWhatAReaderReports: which checkout a pipeline was loaded
// from, for a reader telling two apart.
func (s suite) TestSourcePathIsWhatAReaderReports(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	err := st.SetSourcePath(ctx, "/src/app/pipeline.yml")
	if err != nil {
		t.Fatalf("SetSourcePath: %v", err)
	}

	rows, err := st.Reader().Pipelines(ctx)
	if err != nil || len(rows) != 1 || rows[0].Path != "/src/app/pipeline.yml" {
		t.Fatalf("Pipelines = %+v, %v; want test at /src/app/pipeline.yml", rows, err)
	}
}

// TestRunCostTotalsKeepsUnpricedApartFromFree: a run whose every step went
// unpriced has no dollar total at all, and one with some priced steps says
// how many were not.
func (s suite) TestRunCostTotalsKeepsUnpricedApartFromFree(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")
	priced := 0.25

	for _, spend := range []struct {
		run, hash string
		cost      *float64
		tokens    int
	}{
		{"mixed", hashOf(1), &priced, 100},
		{"mixed", hashOf(2), nil, 50},
		{"unpriced", hashOf(3), nil, 10},
	} {
		ensureRun(ctx, t, st, spend.run, "review")
		mustRecordNode(t, st, "review", spend.hash)

		err := st.RecordAgentUsage(ctx, store.AgentUsage{
			RunID: spend.run, JobName: "review", NodeHash: spend.hash, Total: spend.tokens, CostUSD: spend.cost,
		})
		if err != nil {
			t.Fatalf("RecordAgentUsage: %v", err)
		}
	}

	totals, err := st.RunCostTotals(ctx, 0)
	if err != nil || len(totals) != 2 {
		t.Fatalf("RunCostTotals = %+v, %v; want two runs", totals, err)
	}

	// Newest first: the unpriced run's usage was recorded last.
	assertTotals(t, totals[0], store.RunTotals{RunID: "unpriced", Tokens: 10, Steps: 1, Unpriced: 1})
	assertTotals(t, totals[1], store.RunTotals{RunID: "mixed", Tokens: 150, Steps: 2, Unpriced: 1, CostUSD: &priced})
}

func assertTotals(t *testing.T, got, want store.RunTotals) {
	t.Helper()

	gotCost, wantCost := got.CostUSD, want.CostUSD
	got.CostUSD, want.CostUSD = nil, nil

	if got != want || (gotCost == nil) != (wantCost == nil) || (gotCost != nil && *gotCost != *wantCost) {
		t.Errorf("run totals %+v (cost %v); want %+v (cost %v) — an unpriced run has no cost at all, not $0", got, gotCost, want, wantCost)
	}
}

// TestCheckedResourcesListsEachCursor, by name.
func (s suite) TestCheckedResourcesListsEachCursor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	for name, version := range map[string]string{"repo": `{"ref":"b"}`, "image": `{"digest":"a"}`} {
		err := st.RecordCheckedVersion(ctx, name, version)
		if err != nil {
			t.Fatalf("RecordCheckedVersion: %v", err)
		}
	}

	checked, err := st.CheckedResources(ctx)
	if err != nil || len(checked) != 2 || checked[0].Name != "image" || checked[1].Version != `{"ref":"b"}` || checked[0].CheckedAt.IsZero() {
		t.Fatalf("CheckedResources = %+v, %v; want image then repo, each with its version and when", checked, err)
	}
}

// TestHasPassedVersionSetNeedsOneBuild: versions that each passed in
// different builds were never proven together.
func (s suite) TestHasPassedVersionSetNeedsOneBuild(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")
	want := map[string]string{"repo": `{"ref":"a"}`, "image": `{"digest":"b"}`}

	pass := func(resource, build string) {
		t.Helper()

		err := st.RecordPassedVersion(ctx, "unit", resource, want[resource], build)
		if err != nil {
			t.Fatalf("RecordPassedVersion: %v", err)
		}
	}

	assertPassedSet := func(set map[string]string, expected bool, why string) {
		t.Helper()

		got, err := st.HasPassedVersionSet(ctx, "unit", set)
		if err != nil || got != expected {
			t.Fatalf("HasPassedVersionSet = %v, %v; want %v — %s", got, err, expected, why)
		}
	}

	assertPassedSet(nil, true, "no constraint is vacuously met")

	pass("repo", "b1")
	pass("image", "b2")
	assertPassedSet(want, false, "each passed, in different builds")

	pass("image", "b1")
	assertPassedSet(want, true, "one build passed both")
}

// TestRecordRunParentIsWhatARunReports: a replay says which run it forked.
func (s suite) TestRecordRunParentIsWhatARunReports(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	mustStartRuns(ctx, t, st, "build", "original", "replay")

	err := st.RecordRunParent(ctx, "replay", "original")
	if err != nil {
		t.Fatalf("RecordRunParent: %v", err)
	}

	run, found, err := st.FindRunRow(ctx, "replay")
	if err != nil || !found || run.ParentRunID != "original" {
		t.Fatalf("FindRunRow = %+v, %v, %v; want a parent of original", run, found, err)
	}
}
