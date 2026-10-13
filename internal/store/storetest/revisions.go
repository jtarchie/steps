package storetest

// The configuration a run was served, as a caller of the contract sees it.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// setRevision records a configuration and makes it the pipeline's, which is what a `steps pipeline set` does to the database.
func setRevision(t *testing.T, st store.Store, sha string, edit int, includes map[string]string) {
	t.Helper()

	err := st.RecordRevision(ctxFor(t), sha, pipelineSource(edit), includes)
	if err != nil {
		t.Fatalf("RecordRevision(%s): %v", sha, err)
	}

	err = st.SetCurrentRevision(ctxFor(t), sha, "/src/app/pipeline.yml")
	if err != nil {
		t.Fatalf("SetCurrentRevision(%s): %v", sha, err)
	}
}

// TestTheCurrentRevisionIsWhatADaemonServes: a set records a configuration and names it current, and a restart has nothing else to rebuild from.
func (s suite) TestTheCurrentRevisionIsWhatADaemonServes(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	// Nothing set yet, which is the state a pipeline that only ever ran by hand is in — and the daemon serves nothing for it.
	_, found, err := st.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision: %v", err)
	}

	if found {
		t.Error("a pipeline nobody set reports a current configuration")
	}

	setRevision(t, st, "sha-one", 1, map[string]string{"ci/build.sh": "echo building\n"})

	current, found, err := st.CurrentRevision(ctx)
	if err != nil || !found {
		t.Fatalf("CurrentRevision after a set = (%v, %v)", found, err)
	}

	if current.SHA != "sha-one" || current.Source != pipelineSource(1) {
		t.Errorf("CurrentRevision = %q/%q, want the configuration that was set", current.SHA, current.Source)
	}

	// The includes travel with it, or a daemon restarting resolves a run_file: against a filesystem the sender never had.
	if current.Includes["ci/build.sh"] != "echo building\n" {
		t.Errorf("CurrentRevision carries includes %v, want the ones recorded with it", current.Includes)
	}

	// A later set moves it, so the daemon serves the newest rather than the first.
	setRevision(t, st, "sha-two", 2, nil)

	current, _, err = st.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision: %v", err)
	}

	if current.SHA != "sha-two" {
		t.Errorf("CurrentRevision = %q after a second set, want sha-two", current.SHA)
	}
}

// TestSettingARevisionNobodyRecordedIsRefused: pointing at nothing would leave a daemon restarting into a pipeline it serves no configuration for, which reads as a pipeline nobody set.
func (s suite) TestSettingARevisionNobodyRecordedIsRefused(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	err := st.SetCurrentRevision(ctx, "sha-nobody-recorded", "/src/app/pipeline.yml")
	if err == nil {
		t.Fatal("a revision that was never recorded was accepted as the current one")
	}

	_, found, err := st.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision: %v", err)
	}

	if found {
		t.Error("a refused set left a current configuration behind")
	}
}

// TestAnUnreadableCurrentRevisionIsAnError: "nothing set" and "could not look" are different answers, and a daemon restarting on the first quietly serves nothing for a pipeline somebody set.
func (s suite) TestAnUnreadableCurrentRevisionIsAnError(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	setRevision(t, st, "sha-one", 1, nil)

	ctx, cancel := context.WithCancel(ctxFor(t))
	cancel()

	_, found, err := st.CurrentRevision(ctx)
	if err == nil {
		t.Fatalf("CurrentRevision on a read that could not happen = (found %v, nil), want an error", found)
	}
}

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
	err := st.StartRun(ctx, store.NewRun{ID: "run-one", JobName: "build", Workspace: "/tmp/ws-one", ConfigSHA: "sha-one"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = st.StartRun(ctx, store.NewRun{ID: "run-two", JobName: "build", Workspace: "/tmp/ws-two", ConfigSHA: "sha-two"})
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

	err := st.StartRun(ctx, store.NewRun{ID: "run-one", JobName: "build", Workspace: "/tmp/ws", ConfigSHA: "sha-broken"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = st.FinishRun(ctx, "run-one", store.RunFailed)
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

	err := st.StartRun(ctx, store.NewRun{ID: "run-one", JobName: "build", Workspace: "/tmp/ws-one"})
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

	err = st.StartRun(ctx, store.NewRun{ID: "run-one", JobName: "build", Workspace: "/tmp/ws", ConfigSHA: "sha-recorded"})
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

// TestIncludesComeBackByteForByte: a daemon restart rebuilds a pipeline from
// these rows alone, under the sha that covers them, so an include that is not
// UTF-8 text — a Latin-1 script, a NUL — must come back as it went in. A
// driver that "cleaned" it would run a script the sha never described.
func (s suite) TestIncludesComeBackByteForByte(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	includes := map[string]string{
		"ci/build.sh": "echo caf\xe9\x00done\n",
		"ci/test.sh":  "echo test\n",
		"ci/lint.sh":  "echo lint\n",
	}

	err := st.RecordRevision(ctx, "sha-one", pipelineSource(1), includes)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	rev, found, err := st.FindRevision(ctx, "sha-one")
	if err != nil || !found {
		t.Fatalf("FindRevision: found=%v err=%v", found, err)
	}

	if len(rev.Includes) != len(includes) {
		t.Errorf("%d includes came back, want all %d", len(rev.Includes), len(includes))
	}

	for path, content := range includes {
		if got := rev.Includes[path]; got != content {
			t.Errorf("include %s came back as %q, want %q", path, got, content)
		}
	}
}
