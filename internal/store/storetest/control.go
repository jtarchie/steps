package storetest

// What a `steps pipeline` verb does to the database, as a caller of the contract sees it.

import (
	"slices"
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

// TestTheCurrentRevisionIsScopedToItsPipeline: a state file holds several, and one daemon row answering with another's configuration would serve the wrong pipeline entirely.
func (s suite) TestTheCurrentRevisionIsScopedToItsPipeline(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	mine := s.open(t, "test")
	theirs := s.open(t, "other")

	setRevision(t, theirs, "sha-theirs", 1, nil)

	_, found, err := mine.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision: %v", err)
	}

	if found {
		t.Error("one pipeline reports the configuration another pipeline was set with")
	}
}

// TestPauseIsTheWholePipelinesBreaker: it survives a re-read, so a drain in another goroutine reads the same answer the verb wrote.
func (s suite) TestPauseIsTheWholePipelinesBreaker(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	paused, err := st.Paused(ctx)
	if err != nil {
		t.Fatalf("Paused: %v", err)
	}

	if paused {
		t.Fatal("a new pipeline is paused")
	}

	err = st.Pause(ctx)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	paused, err = st.Paused(ctx)
	if err != nil || !paused {
		t.Fatalf("Paused after Pause = (%v, %v), want true", paused, err)
	}

	err = st.Unpause(ctx)
	if err != nil {
		t.Fatalf("Unpause: %v", err)
	}

	paused, err = st.Paused(ctx)
	if err != nil || paused {
		t.Fatalf("Paused after Unpause = (%v, %v), want false", paused, err)
	}
}

// TestPauseIsScopedToItsPipeline: a shared state file would otherwise stop every pipeline in it when one was paused.
func (s suite) TestPauseIsScopedToItsPipeline(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	mine := s.open(t, "test")
	theirs := s.open(t, "other")

	err := theirs.Pause(ctx)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	paused, err := mine.Paused(ctx)
	if err != nil {
		t.Fatalf("Paused: %v", err)
	}

	if paused {
		t.Error("pausing one pipeline paused another sharing the state file")
	}
}

// TestRenameKeepsTheHistoryUnderTheNewName: every scoped row reaches the pipeline by id, so a rename is one UPDATE rather than a new identity with empty state.
func (s suite) TestRenameKeepsTheHistoryUnderTheNewName(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	err := st.StartRun(ctx, "run-one", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = st.FinishRun(ctx, "run-one", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	err = st.Rename(ctx, "renamed")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}

	held := pipelineNames(t, st)
	if slices.Contains(held, "test") {
		t.Error("the old name is still in the file, so a rename minted a second pipeline")
	}

	if !slices.Contains(held, "renamed") {
		t.Fatalf("no pipeline is called renamed after a rename: %v", held)
	}

	// The run is the renamed pipeline's, which is the whole point of the identity being a row rather than a string.
	runs, err := st.ListRuns(ctx, "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(runs) != 1 || runs[0].ID != "run-one" {
		t.Errorf("after a rename the pipeline reports %+v, want the run it recorded before", runs)
	}
}

// TestDeleteForgetsThePipelineAndItsHistory: destroy is one DELETE and everything scoped to the pipeline goes with it, including the revision the RESTRICT would otherwise hold.
func (s suite) TestDeleteForgetsThePipelineAndItsHistory(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	doomed := s.open(t, "test")
	bystander := s.open(t, "other")

	setRevision(t, doomed, "sha-one", 1, map[string]string{"ci/build.sh": "echo hi\n"})

	err := doomed.StartRun(ctx, "run-one", "build", "/tmp/ws", "sha-one")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = bystander.StartRun(ctx, "run-two", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = doomed.Delete(ctx)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if slices.Contains(pipelineNames(t, bystander), "test") {
		t.Error("the deleted pipeline is still in the file")
	}

	// The neighbour keeps everything, which is what makes destroy safe in a shared state file.
	runs, err := bystander.ListRuns(ctx, "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(runs) != 1 || runs[0].ID != "run-two" {
		t.Errorf("the neighbouring pipeline reports %+v, want the run it recorded", runs)
	}
}

// TestPipelinesReportWhatADaemonWouldServe: the listing carries the current sha and the pause state, so a daemon starting up knows which rows to serve without opening each one.
func (s suite) TestPipelinesReportWhatADaemonWouldServe(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	set := s.open(t, "test")
	unset := s.open(t, "other")

	setRevision(t, set, "sha-one", 1, nil)

	err := set.Pause(ctx)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	rows, err := unset.Reader().Pipelines(ctx)
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}

	byName := map[string]store.PipelineRow{}
	for _, row := range rows {
		byName[row.Name] = row
	}

	if byName["test"].CurrentSHA != "sha-one" || !byName["test"].Paused {
		t.Errorf("the set, paused pipeline reports %+v", byName["test"])
	}

	if byName["other"].CurrentSHA != "" || byName["other"].Paused {
		t.Errorf("a pipeline nobody set or paused reports %+v", byName["other"])
	}
}

// pipelineNames is what the file holds, by name.
func pipelineNames(t *testing.T, st store.Store) []string {
	t.Helper()

	rows, err := st.Reader().Pipelines(ctxFor(t))
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}

	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}

	return names
}

// TestRetentionNeverReapsTheConfigurationBeingServed is the guard a restart
// rests on: a daemon rebuilds every served pipeline from its current
// revision, so reaping that row leaves the pipeline serving nothing.
//
// The case is not hypothetical and not the newest-row exemption in disguise.
// A `steps run` against the same database interns a revision of its own, and
// its loaded_at is newer — so the row the daemon is serving stops being the
// most recently loaded one, which is the only other thing keeping it.
func (s suite) TestRetentionNeverReapsTheConfigurationBeingServed(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	setRevision(t, st, "sha-served", 1, map[string]string{"ci/build.sh": "echo served\n"})

	// A local run of the same pipeline, loaded after the set and referenced by
	// nothing that survives the sweep.
	err := st.RecordRevision(ctx, "sha-ran-locally", pipelineSource(2), nil)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	err = st.Prune(ctx, store.Retention{}, "")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	current, found, err := st.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision: %v", err)
	}

	if !found {
		t.Fatal("retention reaped the configuration the daemon is serving; a restart would serve nothing")
	}

	if current.SHA != "sha-served" || current.Includes["ci/build.sh"] != "echo served\n" {
		t.Errorf("the served configuration came back as %q with includes %v", current.SHA, current.Includes)
	}
}
