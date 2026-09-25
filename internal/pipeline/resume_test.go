package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/merkle"
)

// TestCheckResumedBuildsLinesUpEachBuild: a build's record is trusted only at
// the position it was made in. The e2e covers a version moving; this covers
// the builds a resume no longer resolves at all, and the growth it allows.
func TestCheckResumedBuildsLinesUpEachBuild(t *testing.T) {
	t.Parallel()

	ctx := withResume(context.Background(), &resumeState{id: "R", resuming: true})

	set := func(n string) merkle.InputSet {
		return merkle.InputSet{"ticks": {"n": n}}
	}

	resolved := func(sets ...merkle.InputSet) setResolution {
		return setResolution{sets: sets, everyInputs: []everyInput{{input: "ticks", resource: "ticks"}}}
	}

	recorded := map[string]map[string]string{
		"R#0": {"ticks": `{"n":"one"}`},
		"R#1": {"ticks": `{"n":"two"}`},
	}

	err := checkResumedBuilds(ctx, resolved(set("one"), set("two"), set("three")), recorded)
	if err != nil {
		t.Errorf("a version sorting after the recorded ones was refused: %v", err)
	}

	err = checkResumedBuilds(ctx, resolved(set("one")), recorded)
	if err == nil || !strings.Contains(err.Error(), "build #1") {
		t.Errorf("a recorded build that no longer resolves: want a refusal naming build #1, got %v", err)
	}

	err = checkResumedBuilds(ctx, resolved(set("two"), set("one")), recorded)
	if err == nil || !strings.Contains(err.Error(), "build #0") {
		t.Errorf("reordered builds: want a refusal naming build #0, got %v", err)
	}
}
