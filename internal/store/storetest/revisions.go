package storetest

// The configuration a run was served, as a caller of the contract sees it.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// pipelineSource is a plausible pipeline YAML of a few kilobytes, which is
// what a revision row actually costs — the measurements below are meaningless
// against a 20-byte stand-in.
func pipelineSource(edit int) string {
	return fmt.Sprintf("# revision %d\njobs:\n- name: build\n  plan:\n  - task: compile\n    run: |\n      %s\n",
		edit, strings.Repeat("echo building; ", 200))
}

// TestRunsRecordTheRevisionTheyWereGiven is the correction that removed the
// revision from this handle: a run names the configuration IT was handed,
// which is not necessarily the newest one this pipeline has loaded.
//
// Every argument order is exercised, because the defect it replaces was an
// ordering one — the row was written long after the caller took its config,
// so whichever revision happened to be newest at write time won.
func (s suite) TestRunsRecordTheRevisionTheyWereGiven(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	for _, sha := range []string{"sha-one", "sha-two"} {
		err := st.RecordRevision(ctx, sha, pipelineSource(1), nil)
		if err != nil {
			t.Fatalf("RecordRevision(%s): %v", sha, err)
		}
	}

	// Started under the OLDER one, with the newer already interned — the
	// daemon reloaded while this run was getting under way.
	err := st.StartRun(ctx, "run-one", "build", "/tmp/ws-one", "sha-one")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = st.StartRun(ctx, "run-two", "build", "/tmp/ws-two", "sha-two")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	got := map[string]string{}

	rows, err := st.ListRuns(ctx, "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	for _, row := range rows {
		got[row.ID] = row.ConfigSHA
	}

	for id, want := range map[string]string{"run-one": "sha-one", "run-two": "sha-two"} {
		if got[id] != want {
			t.Errorf("run %s reports configuration %q, want %q", id, got[id], want)
		}
	}
}

// TestResumeRecordsTheConfigItResumesUnder: a resume continues a failed run
// under the configuration it is being resumed WITH, which is usually the one
// that fixed it. Keeping the original would make the run claim it executed a
// pipeline nothing in it ever ran.
func (s suite) TestResumeRecordsTheConfigItResumesUnder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	for _, sha := range []string{"sha-broken", "sha-fixed"} {
		err := st.RecordRevision(ctx, sha, pipelineSource(1), nil)
		if err != nil {
			t.Fatalf("RecordRevision(%s): %v", sha, err)
		}
	}

	err := st.StartRun(ctx, "run-one", "build", "/tmp/ws", "sha-broken")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = st.FinishRun(ctx, "run-one", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	err = st.ResumeRun(ctx, "run-one", "/tmp/ws", "sha-fixed")
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}

	rows, err := st.ListRuns(ctx, "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(rows) != 1 || rows[0].ConfigSHA != "sha-fixed" {
		t.Errorf("after a resume the run reports %+v, want sha-fixed", rows)
	}
}

// TestRunWithNoRecordedConfigurationSaysSo pins the NULL: a caller that
// loaded no pipeline file records no revision, and its runs must still be
// insertable — the column is a foreign key, and 0 is not a row id.
func (s suite) TestRunWithNoRecordedConfigurationSaysSo(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	err := st.StartRun(ctx, "run-one", "build", "/tmp/ws-one", "")
	if err != nil {
		t.Fatalf("StartRun with no configuration recorded: %v", err)
	}

	rows, err := st.ListRuns(ctx, "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(rows) != 1 || rows[0].ConfigSHA != "" {
		t.Errorf("a run started with no configuration reports %+v, want one row with an empty ConfigSHA", rows)
	}
}

// TestFindRevisionIsScopedToItsPipeline: a state file may hold several
// pipelines, and a hash identifies bytes rather than a pipeline — so an
// unscoped lookup would serve one pipeline's page a configuration another
// pipeline ran, which is the shape of bug every query in this package carries
// a pipeline_id predicate to prevent.
func (s suite) TestFindRevisionIsScopedToItsPipeline(t *testing.T) {
	t.Parallel()

	mine := s.open(t, "test")

	theirs := s.open(t, "other")

	err := theirs.RecordRevision(ctxFor(t), "sha-theirs", pipelineSource(1), nil)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	_, found, err := mine.FindRevision(ctxFor(t), "sha-theirs")
	if err != nil {
		t.Fatalf("FindRevision: %v", err)
	}

	if found {
		t.Error("one pipeline read a configuration another pipeline recorded")
	}

	// And its own is still found, so the scoping is a predicate rather than a
	// lookup that never works.
	err = mine.RecordRevision(ctxFor(t), "sha-mine", pipelineSource(2), nil)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	revision, found, err := mine.FindRevision(ctxFor(t), "sha-mine")
	if err != nil || !found {
		t.Fatalf("FindRevision of its own configuration = (%v, %v, %v)", revision.SHA, found, err)
	}

	if revision.Source != pipelineSource(2) {
		t.Error("FindRevision returned a configuration that is not the one recorded")
	}
}

// ctxFor is the test's context, named so the calls above read as one line.
func ctxFor(t *testing.T) context.Context {
	t.Helper()

	return t.Context()
}

// TestResumeKeepsTheConfigurationItCannotName: a resume writes the
// configuration it is resumed WITH, and a subselect that matches nothing is
// not one. Assigning it turned "this run executed that" into "this run
// executed nothing", which is the single answer the column exists to deny.
func (s suite) TestResumeKeepsTheConfigurationItCannotName(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	err := st.RecordRevision(ctx, "sha-recorded", pipelineSource(1), nil)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	err = st.StartRun(ctx, "run-one", "build", "/tmp/ws", "sha-recorded")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// A sha this pipeline has no row for: swept, or a caller that loaded no
	// file at all.
	err = st.ResumeRun(ctx, "run-one", "/tmp/ws", "sha-nobody-recorded")
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}

	rows, err := st.ListRuns(ctx, "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(rows) != 1 || rows[0].ConfigSHA != "sha-recorded" {
		t.Errorf("after a resume the run reports configuration %q, want it to keep %q",
			rows[0].ConfigSHA, "sha-recorded")
	}
}
