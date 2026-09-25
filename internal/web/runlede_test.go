package web

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// TestFailedRunNamesItsInnermostFailure: the header links the step that
// actually broke — the same row the f key finds — not the container that is
// red only because of it. A reader who does not know the key gets there in
// one click without scrolling.
func TestFailedRunNamesItsInnermostFailure(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-fail", "build", t.TempDir(), "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-fail", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "checks", StepKind: "in_parallel", StepID: 1},
		{Type: events.TypeStepStarted, StepIndex: 1, StepName: "lint", StepKind: "task", StepID: 2, ParentStepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 1, StepName: "lint", StepKind: "task", StepID: 2, ParentStepID: 1, Status: "succeeded"},
		{Type: events.TypeStepStarted, StepIndex: 2, StepName: "unit tests", StepKind: "task", StepID: 3, ParentStepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 2, StepName: "unit tests", StepKind: "task", StepID: 3, ParentStepID: 1, Status: "failed"},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "checks", StepKind: "in_parallel", StepID: 1, Status: "failed"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-fail", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-fail")

	if !strings.Contains(body, `failed at <a href="#step-3-unit-tests">unit tests</a>`) {
		t.Errorf("run page does not lead with the innermost failure: %s", body)
	}

	if strings.Contains(body, `failed at <a href="#step-1-checks">`) {
		t.Error("the lede names the container rather than the step that broke")
	}
}

func TestAGreenRunHasNoFailureLede(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	startFinishedRun(t, pipeline, "run-ok", "build", "succeeded")

	_, body := get(t, server, "/p/demo/runs/run-ok")
	if strings.Contains(body, "failed at") {
		t.Error("a green run carries a failure lede")
	}
}

// TestRunPageNamesTheJobsNeighbours: the job page's dependency block, as one
// line on the run — each neighbour is a job link, which resolves to its
// latest run.
func TestRunPageNamesTheJobsNeighbours(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	startFinishedRun(t, pipeline, "run-b", "build", "succeeded")
	startFinishedRun(t, pipeline, "run-d", "deploy", "succeeded")

	_, build := get(t, server, "/p/demo/runs/run-b")
	if !strings.Contains(build, `feeds <a href="/p/demo/jobs/deploy">deploy</a> <span class="res">[repo]</span>`) {
		t.Errorf("build's run does not say what it feeds: %s", build)
	}

	if strings.Contains(build, "after <a") {
		t.Error("build has no upstream but the run says it does")
	}

	_, deploy := get(t, server, "/p/demo/runs/run-d")
	if !strings.Contains(deploy, `after <a href="/p/demo/jobs/build">build</a> <span class="res">[repo]</span>`) {
		t.Errorf("deploy's run does not say what it runs after: %s", deploy)
	}
}
