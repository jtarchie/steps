package web

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// The error is read on the step that raised it, and the head only names that step: the block that used to lead the page repeated the failing step's own text and pushed the transcript below the fold on the page a reader opens to triage.
func TestTheFailingStepHoldsTheRunsError(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, store.NewRun{ID: "run-held", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-held", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "boom", StepKind: "task", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "boom", StepKind: "task", StepID: 1, Status: "failed", Text: "exit status 3"},
		{Type: events.TypeJobFinished, Status: "failed", Text: "step 1 (task boom): exit status 3"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-held", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-held")

	if strings.Contains(body, "errblock") {
		t.Errorf("an error the failing step holds is repeated above the transcript:\n%s", body)
	}

	stepBody := strings.Index(body, `id="step-1-boom_body"`)
	errText := strings.Index(body, `<span class="err">exit status 3</span>`)

	if stepBody < 0 || errText < stepBody {
		t.Errorf("the failing step does not show its error in its body (body at %d, error at %d):\n%s", stepBody, errText, body)
	}

	if strings.Count(body, "exit status 3") != 1 {
		t.Errorf("the error appears %d times, want once", strings.Count(body, "exit status 3"))
	}
}

// A run that died before any step failed — an image pull, a placement, a resource check — has no row to carry its error, so the head keeps it.
func TestAnErrorNoStepHoldsStaysInTheHead(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, store.NewRun{ID: "run-headless", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-headless", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "ok", StepKind: "task", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "ok", StepKind: "task", StepID: 1, Status: "succeeded"},
		{Type: events.TypeJobFinished, Status: "failed", Text: "place step 2: no worker for tag gpu"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-headless", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-headless")

	if !strings.Contains(body, `errblock"><span class="lbl">job error:</span> place step 2: no worker for tag gpu`) {
		t.Errorf("an error no step holds is not shown in the head:\n%s", body)
	}
}

// A server that answers but lacks a tool the grant names is not a connection problem, and "is it connected?" sent the reader to an mcp row that said green. The hint says what is actually wrong instead.
func TestAMissingToolIsNotHintedAsAConnection(t *testing.T) {
	t.Parallel()

	server, _, _ := mcpServerPipeline(t, stubRunner{})
	pipeline := server.Lookup("demo")
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, store.NewRun{ID: "run-no-tool", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-no-tool", []store.RunEventRow{
		{Type: events.TypeJobFinished, Status: "errored", Text: "job \"build\": preflight failed, no steps were run:\n  mcp \"linear\": mcp server \"linear\" has no tool named \"read_resource\""},
	})

	err = pipeline.Store.FinishRun(ctx, "run-no-tool", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-no-tool")

	if strings.Contains(body, "is it connected?") {
		t.Errorf("a missing tool is hinted as a connection problem:\n%s", body)
	}

	if !strings.Contains(body, `<code>linear</code> answers, but has no tool named <code>read_resource</code>`) {
		t.Errorf("the hint does not say the tool is missing:\n%s", body)
	}

	if !strings.Contains(body, "This run recorded no steps.") {
		t.Errorf("a refused run does not say it ran nothing:\n%s", body)
	}
}

// The "is it connected?" hint follows the error to the step that holds it, since that is now where the reader meets the server's name.
func TestAnMCPFailureAStepHoldsIsHintedOnTheStep(t *testing.T) {
	t.Parallel()

	server, _, _ := mcpServerPipeline(t, stubRunner{})
	pipeline := server.Lookup("demo")
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, store.NewRun{ID: "run-mcp-step", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-mcp-step", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "triage", StepKind: "agent", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "triage", StepKind: "agent", StepID: 1, Status: "failed", Text: `mcp server "linear" is not authorized`},
		{Type: events.TypeJobFinished, Status: "failed", Text: `step 1 (agent triage): mcp server "linear" is not authorized`},
	})

	err = pipeline.Store.FinishRun(ctx, "run-mcp-step", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-mcp-step")

	if strings.Contains(body, "errblock") {
		t.Errorf("an mcp error the failing step holds is repeated above the transcript:\n%s", body)
	}

	stepBody := strings.Index(body, `id="step-1-triage_body"`)
	hint := strings.Index(body, `href="/p/demo/mcp#mcp-linear"`)

	if stepBody < 0 || hint < stepBody {
		t.Errorf("the mcp hint did not follow the error into the step (body at %d, hint at %d):\n%s", stepBody, hint, body)
	}
}
