package pipeline

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// fakeRunInputs is the Versions facet reduced to the one read a resume makes.
type fakeRunInputs struct {
	store.Versions

	inputs []store.RunInput
}

func (f fakeRunInputs) RunInputs(context.Context, string) ([]store.RunInput, error) {
	return f.inputs, nil
}

func ticksHistory(versions ...string) *resourceHistory {
	history := &resourceHistory{versions: map[string][]map[string]any{}, gated: map[string]bool{}}
	for _, n := range versions {
		history.versions["ticks"] = append(history.versions["ticks"], map[string]any{"n": n})
	}

	history.versions["config"] = []map[string]any{{"n": "c1"}}

	return history
}

// recordedRun is a run's record: the fanning get and a fixed get beside it.
func recordedRun() []store.RunInput {
	return []store.RunInput{
		{Input: "ticks", Resource: "ticks", Version: `{"n":"one"}`},
		{Input: "config", Resource: "config", Version: `{"n":"c1"}`},
	}
}

func resumeRecorded(done map[doneKey]doneStep, inputs []store.RunInput, history *resourceHistory) (setResolution, error) {
	ctx := withResume(context.Background(), &resumeState{id: "R", resuming: true, done: done})
	fresh := setResolution{everyInputs: []everyInput{{input: "ticks", resource: "ticks"}}}

	return resumeInputSets(ctx, fakeRunInputs{inputs: inputs}, fresh, history)
}

// The e2e covers a version moving and one pruned; these cover the shapes a
// record can be in.

// A resume's set is the run's own record, every get bound.
func TestResumeInputSetsRebuildsTheRecordedBuild(t *testing.T) {
	t.Parallel()

	got, err := resumeRecorded(map[doneKey]doneStep{{"R#0", 0, 1}: {name: "fragile"}}, recordedRun(), ticksHistory("one", "two", "three"))
	if err != nil {
		t.Fatalf("a recorded run was refused: %v", err)
	}

	if !got.recorded || len(got.sets) != 1 {
		t.Fatalf("sets = %v, want the recorded build", got.sets)
	}

	if got.sets[0]["ticks"]["n"] != "one" || got.sets[0]["config"]["n"] != "c1" {
		t.Errorf("sets = %v, want every get bound to its record", got.sets)
	}
}

// A run that failed before its build was created has nothing to rebuild and resolves afresh.
func TestResumeInputSetsResolvesAfreshBeforeTheFirstBuild(t *testing.T) {
	t.Parallel()

	got, err := resumeRecorded(map[doneKey]doneStep{{"R", 0, 1}: {name: "prep"}}, nil, ticksHistory("one"))
	if err != nil || got.recorded || got.sets != nil {
		t.Errorf("want the fresh resolution back, got %v, %v", got, err)
	}
}

// Steps completed under a build nothing recorded cannot be placed.
func TestResumeInputSetsRefusesABuildWithStepsButNoRecord(t *testing.T) {
	t.Parallel()

	_, err := resumeRecorded(map[doneKey]doneStep{{"R#0", 0, 1}: {name: "fragile"}}, nil, ticksHistory("one", "two"))
	if err == nil || !strings.Contains(err.Error(), "not recorded") {
		t.Errorf("want a refusal naming the missing record, got %v", err)
	}
}

// A version history no longer holds is refused, naming the version.
func TestResumeInputSetsRefusesAPrunedVersion(t *testing.T) {
	t.Parallel()

	_, err := resumeRecorded(nil, recordedRun(), ticksHistory("two", "three"))
	if err == nil || !strings.Contains(err.Error(), `{"n":"one"}`) {
		t.Errorf("want a refusal naming the version, got %v", err)
	}
}

func TestProgressedPast(t *testing.T) {
	t.Parallel()

	done := map[doneKey]doneStep{{"R#0", 2, 1}: {name: "change"}}

	for name, tc := range map[string]struct {
		state *resumeState
		build string
		index int
		want  bool
	}{
		"no state":                {nil, "R#0", -1, false},
		"not resuming":            {&resumeState{id: "R", done: done}, "R#0", -1, false},
		"the walk before the get": {&resumeState{id: "R", done: done, resuming: true}, "R", -1, false},
		"the step itself":         {&resumeState{id: "R", done: done, resuming: true}, "R#0", 2, false},
		"a step after it":         {&resumeState{id: "R", done: done, resuming: true}, "R#0", 1, true},
		"the triggered build":     {&resumeState{id: "R", done: done, resuming: true}, "R#0", -1, true},
	} {
		if got := tc.state.progressedPast(tc.build, tc.index); got != tc.want {
			t.Errorf("%s: progressedPast(%s, %d) = %v, want %v", name, tc.build, tc.index, got, tc.want)
		}
	}
}

// TestBuildFinished: finished is what replaying the recorded passes says, so a route that jumped past a step, or a loop, reads as the walk took it rather than as unvisited indices.
func TestBuildFinished(t *testing.T) {
	t.Parallel()

	task := func(name string) config.Step { return config.Step{Task: name} }
	routed := func(name, key, target string) config.Step {
		return config.Step{Task: name, To: map[string]string{key: target}}
	}

	for name, tc := range map[string]struct {
		done  map[doneKey]doneStep
		steps []config.Step
		want  bool
	}{
		"every step":                             {map[doneKey]doneStep{{"R#0", 0, 1}: {}, {"R#0", 1, 1}: {}}, []config.Step{task("a"), task("b")}, true},
		"a step left":                            {map[doneKey]doneStep{{"R#0", 0, 1}: {}}, []config.Step{task("a"), task("b")}, false},
		"the same index before the get":          {map[doneKey]doneStep{{"R", 0, 1}: {}}, []config.Step{task("a")}, false},
		"a plan with nothing after the get":      {nil, nil, true},
		"an in-place get is stepped over":        {map[doneKey]doneStep{{"R#0", 1, 1}: {}}, []config.Step{{Get: "more"}, task("b")}, true},
		"a route jumped past a step":             {map[doneKey]doneStep{{"R#0", 0, 1}: {outcome: "success"}, {"R#0", 2, 1}: {outcome: "success"}}, []config.Step{routed("a", "success", "c"), task("b"), task("c")}, true},
		"a route whose target is unvisited":      {map[doneKey]doneStep{{"R#0", 0, 1}: {outcome: "success"}}, []config.Step{routed("a", "success", "c"), task("b"), task("c")}, false},
		"a loop with every pass":                 {map[doneKey]doneStep{{"R#0", 0, 1}: {outcome: "success"}, {"R#0", 1, 1}: {outcome: "failure"}, {"R#0", 0, 2}: {outcome: "success"}, {"R#0", 1, 2}: {outcome: "success"}}, []config.Step{task("bump"), routed("check", "failure", "bump")}, true},
		"a loop interrupted on its second pass":  {map[doneKey]doneStep{{"R#0", 0, 1}: {outcome: "success"}, {"R#0", 1, 1}: {outcome: "failure"}, {"R#0", 0, 2}: {outcome: "success"}}, []config.Step{task("bump"), routed("check", "failure", "bump")}, false},
		"a row without an outcome falls through": {map[doneKey]doneStep{{"R#0", 0, 1}: {}, {"R#0", 1, 1}: {}, {"R#0", 2, 1}: {}}, []config.Step{routed("a", "success", "c"), task("b"), task("c")}, true},
	} {
		if got := buildFinished(tc.done, "R#0", tc.steps); got != tc.want {
			t.Errorf("%s: buildFinished = %v, want %v", name, got, tc.want)
		}
	}
}

// TestRefuseMissingWorkspaceOnlyWhenSomethingWasKept: a resume is refused for a gone tree only when this build kept a step from it and the run named one.
func TestRefuseMissingWorkspaceOnlyWhenSomethingWasKept(t *testing.T) {
	t.Parallel()

	present := t.TempDir()
	gone := filepath.Join(present, "gone")
	kept := map[doneKey]doneStep{{"R#0", 1, 1}: {name: "change"}}
	elsewhere := map[doneKey]doneStep{{"R#1", 1, 1}: {name: "change"}}

	for name, tc := range map[string]struct {
		root    string
		done    map[doneKey]doneStep
		refused bool
	}{
		"kept, tree gone":              {gone, kept, true},
		"kept, tree present":           {present, kept, false},
		"kept, no tree recorded":       {"", kept, false},
		"nothing kept, tree gone":      {gone, elsewhere, false},
		"nothing kept, nothing at all": {gone, nil, false},
	} {
		err := refuseMissingWorkspace("R", tc.root, tc.done, "R#0")
		if (err != nil) != tc.refused {
			t.Errorf("%s: refuseMissingWorkspace = %v, want refused=%v", name, err, tc.refused)
		}
	}
}

// TestResumeJobNameIsTheRecordedRunsJob is what lets `--resume <id>` name no job.
func TestResumeJobNameIsTheRecordedRunsJob(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	st, err := sqlite.OpenStore(filepath.Join(t.TempDir(), "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	err = st.StartRun(ctx, store.NewRun{ID: "R", JobName: "deploy", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	name, err := ResumeJobName(ctx, st, "R")
	if err != nil || name != "deploy" {
		t.Errorf("ResumeJobName = %q, %v; want deploy", name, err)
	}

	_, err = ResumeJobName(ctx, st, "missing")
	if err == nil {
		t.Error("ResumeJobName found a job for a run that was never recorded")
	}
}
