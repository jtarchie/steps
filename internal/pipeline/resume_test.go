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

// recordedRun is a run's record: the fanning get and a fixed get beside it.
func recordedRun() []store.RunInput {
	return []store.RunInput{
		{Input: "ticks", Resource: "ticks", Version: `{"n":"one"}`},
		{Input: "config", Resource: "config", Version: `{"n":"c1"}`},
	}
}

func resumeRecorded(done map[doneKey]string, inputs []store.RunInput, history *resourceHistory) (setResolution, error) {
	ctx := withResume(context.Background(), &resumeState{id: "R", resuming: true, done: done})
	fresh := setResolution{everyInputs: []everyInput{{input: "ticks", resource: "ticks"}}}

	return resumeInputSets(ctx, fakeRunInputs{inputs: inputs}, fresh, history)
}

// The e2e covers a version moving and one pruned; these cover the shapes a
// record can be in.

// A resume's set is the run's own record, every get bound.
func TestResumeInputSetsRebuildsTheRecordedBuild(t *testing.T) {
	t.Parallel()

	got, err := resumeRecorded(map[doneKey]string{{"R#0", 0}: "fragile"}, recordedRun(), ticksHistory("one", "two", "three"))
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

	got, err := resumeRecorded(map[doneKey]string{{"R", 0}: "prep"}, nil, ticksHistory("one"))
	if err != nil || got.recorded || got.sets != nil {
		t.Errorf("want the fresh resolution back, got %v, %v", got, err)
	}
}

// Steps completed under a build nothing recorded cannot be placed.
func TestResumeInputSetsRefusesABuildWithStepsButNoRecord(t *testing.T) {
	t.Parallel()

	_, err := resumeRecorded(map[doneKey]string{{"R#0", 0}: "fragile"}, nil, ticksHistory("one", "two"))
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

	done := map[doneKey]string{{"R#0", 2}: "change"}

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

func TestBuildFinished(t *testing.T) {
	t.Parallel()

	done := map[doneKey]string{{"R#0", 1}: "change", {"R", 2}: "prep"}

	for name, tc := range map[string]struct {
		needed []int
		want   bool
	}{
		"every needed step":                 {[]int{1}, true},
		"a step left":                       {[]int{1, 2}, false},
		"the same index before the get":     {[]int{2}, false},
		"a plan with nothing after the get": {nil, true},
	} {
		if got := buildFinished(done, "R#0", tc.needed); got != tc.want {
			t.Errorf("%s: buildFinished = %v, want %v", name, got, tc.want)
		}
	}
}
