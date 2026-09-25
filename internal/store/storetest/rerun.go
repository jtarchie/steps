package storetest

import (
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestARerunRecordsWhichBuildItReran: every RunRow read carries it, since the job's status and the run page both ask; and reaping the original must not take the rerun with it, because a rerun is a run in its own right.
func (s suite) TestARerunRecordsWhichBuildItReran(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	startRuns(t, st, "ORIGINAL", "RERUN")

	err := st.RecordRunRerun(ctx, "RERUN", "ORIGINAL", 2)
	if err != nil {
		t.Fatalf("RecordRunRerun: %v", err)
	}

	row, ok, err := st.FindRunRow(ctx, "RERUN")
	if err != nil || !ok || row.RerunOf != "ORIGINAL" || row.RerunOfBuild != 2 {
		t.Fatalf("FindRunRow = %+v, %v, %v: want a rerun of ORIGINAL build 2", row, ok, err)
	}

	runs, err := st.ListRuns(ctx, "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	of := map[string]string{}
	for _, run := range runs {
		of[run.ID] = run.RerunOf
	}

	if of["RERUN"] != "ORIGINAL" || of["ORIGINAL"] != "" {
		t.Errorf("ListRuns reads rerun_of as %v, want only RERUN a rerun of ORIGINAL", of)
	}
}

// TestARerunOfAnOldBuildDoesNotBecomeTheJobsStatus is Concourse's rule (build.go's updateLatestCompletedBuildForJob): a rerun counts as the job's latest only when it reruns the latest build, so a green retry of last week's build cannot turn today's red job green.
func (s suite) TestARerunOfAnOldBuildDoesNotBecomeTheJobsStatus(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	startRuns(t, st, "OLD", "CURRENT", "RETRYOLD")

	err := st.RecordRunRerun(ctx, "RETRYOLD", "OLD", 0)
	if err != nil {
		t.Fatalf("RecordRunRerun: %v", err)
	}

	if latest := mustLatest(t, st); latest != "CURRENT" {
		t.Errorf("latest = %s, want CURRENT: a rerun of an older build became the job's status", latest)
	}

	err = st.StartRun(ctx, "RETRYCURRENT", "build", "/tmp/r", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = st.RecordRunRerun(ctx, "RETRYCURRENT", "CURRENT", 0)
	if err != nil {
		t.Fatalf("RecordRunRerun: %v", err)
	}

	if latest := mustLatest(t, st); latest != "RETRYCURRENT" {
		t.Errorf("latest = %s, want RETRYCURRENT: a rerun of the latest build is the job's status", latest)
	}
}

func mustLatest(t *testing.T, st interface {
	LatestRunByJob(ctx context.Context) (map[string]store.RunRow, error)
},
) string {
	t.Helper()

	latest, err := st.LatestRunByJob(context.Background())
	if err != nil {
		t.Fatalf("LatestRunByJob: %v", err)
	}

	return latest["build"].ID
}

func startRuns(t *testing.T, st interface {
	StartRun(ctx context.Context, id, jobName, workspaceDir, configSHA string) error
}, ids ...string,
) {
	t.Helper()

	for _, id := range ids {
		err := st.StartRun(context.Background(), id, "build", "/tmp/"+id, "")
		if err != nil {
			t.Fatalf("StartRun %s: %v", id, err)
		}
	}
}
