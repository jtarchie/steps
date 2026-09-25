package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
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

// recordedRun is a two-build record, #1 recorded first so completion order and build order disagree, with a fixed get beside the fanning one in #0.
func recordedRun() []store.RunInput {
	return []store.RunInput{
		{BuildID: "R#1", Input: "ticks", Resource: "ticks", Version: `{"n":"two"}`},
		{BuildID: "R#0", Input: "ticks", Resource: "ticks", Version: `{"n":"one"}`},
		{BuildID: "R#0", Input: "config", Resource: "config", Version: `{"n":"c1"}`},
	}
}

func resumeRecorded(done map[doneKey]string, inputs []store.RunInput, history *resourceHistory) (setResolution, error) {
	ctx := withResume(context.Background(), &resumeState{id: "R", resuming: true, done: done})
	fresh := setResolution{everyInputs: []everyInput{{input: "ticks", resource: "ticks"}}}

	return resumeInputSets(ctx, fakeRunInputs{inputs: inputs}, fresh, history)
}

// The e2e covers a version moving and one pruned; these cover the shapes a
// record can be in.

// A resume's sets are the run's own record, in build order, every get bound.
func TestResumeInputSetsRebuildsTheRecordedBuilds(t *testing.T) {
	t.Parallel()

	got, err := resumeRecorded(map[doneKey]string{{"R#0", 0}: "fragile"}, recordedRun(), ticksHistory("one", "two", "three"))
	if err != nil {
		t.Fatalf("a recorded run was refused: %v", err)
	}

	if !got.recorded || len(got.sets) != 2 {
		t.Fatalf("sets = %v, want the two recorded builds", got.sets)
	}

	if got.sets[0]["ticks"]["n"] != "one" || got.sets[0]["config"]["n"] != "c1" || got.sets[1]["ticks"]["n"] != "two" {
		t.Errorf("sets = %v, want each build in order with every get bound", got.sets)
	}
}

// A run that failed before any build was created has nothing to rebuild and resolves afresh.
func TestResumeInputSetsResolvesAfreshBeforeTheFirstBuild(t *testing.T) {
	t.Parallel()

	got, err := resumeRecorded(map[doneKey]string{{"R", 0}: "prep"}, nil, ticksHistory("one"))
	if err != nil || got.recorded || got.sets != nil {
		t.Errorf("want the fresh resolution back, got %v, %v", got, err)
	}
}

// Steps completed under a build nothing recorded cannot be placed.
func TestResumeInputSetsRefusesABuildWithStepsButNoRecord(t *testing.T) {
	t.Parallel()

	_, err := resumeRecorded(map[doneKey]string{{"R#2", 0}: "fragile"}, recordedRun(), ticksHistory("one", "two"))
	if err == nil || !strings.Contains(err.Error(), "build #2") {
		t.Errorf("want a refusal naming build #2, got %v", err)
	}
}

// A version history no longer holds is refused, naming the build and the version.
func TestResumeInputSetsRefusesAPrunedVersion(t *testing.T) {
	t.Parallel()

	_, err := resumeRecorded(nil, recordedRun(), ticksHistory("two", "three"))
	if err == nil || !strings.Contains(err.Error(), "build #0") || !strings.Contains(err.Error(), `{"n":"one"}`) {
		t.Errorf("want a refusal naming build #0 and the version, got %v", err)
	}
}
