package storetest

// The resume index across the several pipelines one state file may hold.

import (
	"context"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// TestCompletedRunStepsAreScopedToTheirPipeline pins the read --resume trusts.
//
// run_steps carries no pipeline column of its own, so this read reaches the
// pipeline only through runs. Unscoped, a pipeline asking about a run id it
// does not own reads the OWNER's finished steps as its own and skips work it
// never did — which for the resume index means skipping work, not just
// misreporting it.
//
// Defense in depth since StartRun stopped upserting: a run id now names a row
// in exactly one pipeline, so no honest resume can ask this question. The
// predicate stays because the repo rule is categorical about it.
func (s suite) TestCompletedRunStepsAreScopedToTheirPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	web := s.open(t, "web")
	infra := s.open(t, "infra")

	const shared = "SHARED01"

	err := web.StartRun(ctx, shared, "build", "/tmp/web", "")
	if err != nil {
		t.Fatalf("StartRun web: %v", err)
	}

	err = web.RecordRunStep(ctx, shared, shared+"#0", 0, "compile")
	if err != nil {
		t.Fatalf("RecordRunStep web: %v", err)
	}

	// infra records no run of its own: runs.id is global and StartRun refuses
	// an id another pipeline holds. What is being asked is narrower and is the
	// query's own contract — infra reading a run id it does not own must see
	// nothing, rather than the other pipeline's finished steps.
	done, err := infra.CompletedRunSteps(ctx, shared)
	if err != nil {
		t.Fatalf("CompletedRunSteps infra: %v", err)
	}

	if len(done) != 0 {
		t.Fatalf("infra sees %d completed steps of a run it never finished: %v", len(done), done)
	}
}

// TestRunEventsAreScopedToTheirPipeline: run_events reaches its pipeline
// only through runs, like run_steps, and the Events facet can be held on its
// own — with no FindRunRow to ask first, the read has to refuse the other
// pipeline's run by itself.
func (s suite) TestRunEventsAreScopedToTheirPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	web := s.open(t, "web")
	infra := s.open(t, "infra")

	const shared = "SHARED02"

	err := web.StartRun(ctx, shared, "build", "/tmp/web", "")
	if err != nil {
		t.Fatalf("StartRun web: %v", err)
	}

	err = web.AppendRunEvent(ctx, store.RunEventRow{RunID: shared, Type: "step_started", StepName: "compile", At: time.Now()})
	if err != nil {
		t.Fatalf("AppendRunEvent web: %v", err)
	}

	err = web.RecordRunInput(ctx, shared, "repo", "repo", `{"ref":"abc"}`)
	if err != nil {
		t.Fatalf("RecordRunInput web: %v", err)
	}

	events, err := infra.RunEvents(ctx, shared, 0, 0)
	if err != nil {
		t.Fatalf("RunEvents infra: %v", err)
	}

	if len(events) != 0 {
		t.Fatalf("infra sees %d event(s) of a run it never ran: %+v", len(events), events)
	}

	inputs, err := infra.RunInputs(ctx, shared)
	if err != nil {
		t.Fatalf("RunInputs infra: %v", err)
	}

	if len(inputs) != 0 {
		t.Fatalf("infra sees the inputs of a run it never created: %v", inputs)
	}
}

// TestRunStepsAreKeptPerWalk is #144's key: the walk after a get counts from
// index 0 again, so a key of (run, index) kept the step before the get and
// dropped the one after it — and a resume read the first as both.
func (s suite) TestRunStepsAreKeptPerWalk(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	web := s.open(t, "web")
	infra := s.open(t, "infra")

	const run = "PERBUILD01"

	err := web.StartRun(ctx, run, "build", "/tmp/web", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// The build's step first, so completion order and build-id order disagree.
	for _, step := range []store.RunStep{
		{BuildID: run + "#0", Index: 0, Name: "compile"},
		{BuildID: run, Index: 0, Name: "prep"},
		{BuildID: run + "#0", Index: 0, Name: "compile"},
	} {
		err = web.RecordRunStep(ctx, run, step.BuildID, step.Index, step.Name)
		if err != nil {
			t.Fatalf("RecordRunStep %+v: %v", step, err)
		}
	}

	got, err := web.CompletedRunSteps(ctx, run)
	if err != nil {
		t.Fatalf("CompletedRunSteps: %v", err)
	}

	want := []store.RunStep{
		{BuildID: run + "#0", Index: 0, Name: "compile"},
		{BuildID: run, Index: 0, Name: "prep"},
	}

	if len(got) != len(want) {
		t.Fatalf("CompletedRunSteps = %+v, want %+v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("CompletedRunSteps = %+v, want %+v (in completion order)", got, want)
		}
	}

	other, err := infra.CompletedRunSteps(ctx, run)
	if err != nil {
		t.Fatalf("CompletedRunSteps infra: %v", err)
	}

	if len(other) != 0 {
		t.Fatalf("infra sees another pipeline's run steps: %+v", other)
	}
}

// TestRunInputsAreKeptPerGet: two gets of one resource in a run are two
// records, each under its own name; and a run is created once, so recording
// a get again is the same row.
func (s suite) TestRunInputsAreKeptPerGet(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "web")

	const run = "PERBUILD02"

	err := st.StartRun(ctx, run, "build", "/tmp/web", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	for _, input := range []store.RunInput{
		{Input: "code", Resource: "repo", Version: `{"ref":"abc"}`},
		{Input: "baseline", Resource: "repo", Version: `{"ref":"v1"}`},
		{Input: "code", Resource: "repo", Version: `{"ref":"abc"}`},
	} {
		err = st.RecordRunInput(ctx, run, input.Input, input.Resource, input.Version)
		if err != nil {
			t.Fatalf("RecordRunInput %+v: %v", input, err)
		}
	}

	inputs, err := st.RunInputs(ctx, run)
	if err != nil {
		t.Fatalf("RunInputs: %v", err)
	}

	got := map[store.RunInput]bool{}
	for _, input := range inputs {
		got[input] = true
	}

	want := []store.RunInput{
		{Input: "code", Resource: "repo", Version: `{"ref":"abc"}`},
		{Input: "baseline", Resource: "repo", Version: `{"ref":"v1"}`},
	}

	if len(inputs) != len(want) {
		t.Fatalf("RunInputs = %+v, want one row per get", inputs)
	}

	for _, input := range want {
		if !got[input] {
			t.Errorf("RunInputs = %+v, missing %+v", inputs, input)
		}
	}
}

// TestVersionRunsAreTheRunsThatTookEachVersion: the resource page asks, per
// version, which runs took it — newest first, so the first row for a version
// is where that version stands now. Scoped like every other read: a second
// pipeline with the same job and resource names must see none of it.
func (s suite) TestVersionRunsAreTheRunsThatTookEachVersion(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	web := s.open(t, "web")
	infra := s.open(t, "infra")

	for _, run := range []struct{ id, version, status string }{
		{"VRUNS01", `{"ref":"v1"}`, "aborted"},
		{"VRUNS02", `{"ref":"v1"}`, "succeeded"},
		{"VRUNS03", `{"ref":"v2"}`, "failed"},
	} {
		recordInputRun(t, web, run.id, "repo", run.version, run.status)

		// started_at orders the answer; one run per tick keeps that order
		// the one this test wrote rather than a tie.
		time.Sleep(2 * time.Millisecond)
	}

	recordInputRun(t, web, "VRUNS04", "other", `{"ref":"v1"}`, "succeeded")

	got, err := web.VersionRuns(ctx, "repo")
	if err != nil {
		t.Fatalf("VersionRuns: %v", err)
	}

	want := []store.VersionRun{
		{Version: `{"ref":"v2"}`, Run: store.RunRow{ID: "VRUNS03", JobName: "build", Status: "failed"}},
		{Version: `{"ref":"v1"}`, Run: store.RunRow{ID: "VRUNS02", JobName: "build", Status: "succeeded"}},
		{Version: `{"ref":"v1"}`, Run: store.RunRow{ID: "VRUNS01", JobName: "build", Status: "aborted"}},
	}

	if len(got) != len(want) {
		t.Fatalf("VersionRuns = %+v, want %d rows, only repo's", got, len(want))
	}

	for i := range want {
		g := store.VersionRun{Version: got[i].Version, Run: store.RunRow{ID: got[i].Run.ID, JobName: got[i].Run.JobName, Status: got[i].Run.Status}}
		if g != want[i] {
			t.Errorf("VersionRuns[%d] = %+v, want %+v", i, g, want[i])
		}
	}

	other, err := infra.VersionRuns(ctx, "repo")
	if err != nil {
		t.Fatalf("VersionRuns infra: %v", err)
	}

	if len(other) != 0 {
		t.Errorf("infra sees another pipeline's runs: %+v", other)
	}
}

// recordInputRun records a finished build run that took version of resource.
func recordInputRun(t *testing.T, st store.Store, runID, resource, version, status string) {
	t.Helper()

	ctx := context.Background()

	err := st.StartRun(ctx, runID, "build", "/tmp/web", "")
	if err != nil {
		t.Fatalf("StartRun %s: %v", runID, err)
	}

	err = st.RecordRunInput(ctx, runID, resource, resource, version)
	if err != nil {
		t.Fatalf("RecordRunInput %s: %v", runID, err)
	}

	err = st.FinishRun(ctx, runID, status)
	if err != nil {
		t.Fatalf("FinishRun %s: %v", runID, err)
	}
}
