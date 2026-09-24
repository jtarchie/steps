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

	err = web.RecordRunInput(ctx, shared, shared+"#0", "repo", `{"ref":"abc"}`)
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

// TestRunStepsAreKeptPerBuild is #144: every build of a fan-out walks its
// remainder from index 0, so a key of (run, index) kept build #0's step and
// dropped every later build's — and a resume read build #0's as all of them.
func (s suite) TestRunStepsAreKeptPerBuild(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	web := s.open(t, "web")
	infra := s.open(t, "infra")

	const run = "PERBUILD01"

	err := web.StartRun(ctx, run, "build", "/tmp/web", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// #1 first, so completion order and build-id order disagree.
	for _, step := range []store.RunStep{
		{BuildID: run + "#1", Index: 0, Name: "compile"},
		{BuildID: run + "#0", Index: 0, Name: "compile"},
		{BuildID: run, Index: 0, Name: "prep"},
		{BuildID: run + "#1", Index: 0, Name: "compile"},
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
		{BuildID: run + "#1", Index: 0, Name: "compile"},
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

// TestRunInputsAreKeptPerBuild: two builds created with the same version are
// two records, because a resume checks each build's bindings on its own.
func (s suite) TestRunInputsAreKeptPerBuild(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "web")

	const run = "PERBUILD02"

	err := st.StartRun(ctx, run, "build", "/tmp/web", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	for _, build := range []string{run + "#0", run + "#1", run + "#1"} {
		err = st.RecordRunInput(ctx, run, build, "repo", `{"ref":"abc"}`)
		if err != nil {
			t.Fatalf("RecordRunInput %s: %v", build, err)
		}
	}

	inputs, err := st.RunInputs(ctx, run)
	if err != nil {
		t.Fatalf("RunInputs: %v", err)
	}

	builds := map[string]bool{}
	for _, input := range inputs {
		builds[input.BuildID] = true
	}

	if len(inputs) != 2 || !builds[run+"#0"] || !builds[run+"#1"] {
		t.Fatalf("RunInputs = %+v, want one row per build", inputs)
	}
}
