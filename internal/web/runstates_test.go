package web

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// A live run's spend is a running total, and the head says so: "$1.20" on a run still spending reads as the bill, and the closing reload replaces the number in place rather than moving anything.
func TestALiveRunsSpendSaysSoFar(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-live", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-live", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "review", StepKind: "agent", StepID: 1},
	})

	hash := strings.Repeat("d", 64)
	mustRecordResult(t, pipeline, hash, map[string]any{"response": "so far"})

	err = pipeline.Store.RecordAgentUsage(ctx, store.AgentUsage{
		RunID: "run-live", StepIndex: 0, StepName: "review", JobName: "build",
		NodeHash: hash, ModelReq: "opus", Total: 1_000, FinishReason: "end_turn",
	})
	if err != nil {
		t.Fatalf("RecordAgentUsage: %v", err)
	}

	_, live := get(t, server, "/p/demo/runs/run-live")
	if !strings.Contains(live, `unpriced</span> so far <a href="#spend">`) {
		t.Errorf("a live run's spend entry does not say it is a running total:\n%s", live)
	}

	err = pipeline.Store.FinishRun(ctx, "run-live", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, done := get(t, server, "/p/demo/runs/run-live")
	if strings.Contains(done, "so far") {
		t.Error("a finished run's spend entry still says so far")
	}
}

// A run the machinery broke (errored, not failed) has no failing step to name, so the head carries the error and no lede.
func TestAnErroredRunKeepsItsErrorInTheHead(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-errored", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-errored", []store.RunEventRow{
		{Type: events.TypeJobFinished, Status: "errored", Text: "docker: pull golang:1.25: no such image"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-errored", "errored")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-errored")

	if !strings.Contains(body, `<span class="lbl">job error:</span> docker: pull golang:1.25: no such image`) {
		t.Errorf("an errored run does not lead with what broke:\n%s", body)
	}

	if strings.Contains(body, "failed at") {
		t.Error("an errored run with no steps names a failing step")
	}

	if !strings.Contains(body, "This run recorded no steps.") {
		t.Error("the transcript placeholder is missing on a run with no steps")
	}
}
