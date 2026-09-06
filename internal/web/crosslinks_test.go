package web

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// TestNodePageLinksItsJob: the receipt names its job as a LINK — it was
// plain text, making the page a dead end.
func TestNodePageLinksItsJob(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	record := store.NodeRecord{
		Hash:     "bbbb111122223333",
		Kind:     "task",
		Resource: "compile",
		Content:  map[string]any{"run": "true"},
	}

	err := pipeline.Store.RecordNode(ctx, record, "build", "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode: %v", err)
	}

	_, body := get(t, server, "/p/demo/nodes/"+record.Hash)

	if !strings.Contains(body, `job <a href="/p/demo/jobs/build">build</a>`) {
		t.Error("node page does not link its job")
	}
}

// TestNodePageDropsJobLinkForAJoblessNode: handleNode's crumbs already guard
// an empty JobName; the metaline link has to match, or a jobless node renders
// an invisible zero-text anchor pointing at /p/demo/jobs/.
func TestNodePageDropsJobLinkForAJoblessNode(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	record := store.NodeRecord{
		Hash:     "dddd111122223333",
		Kind:     "task",
		Resource: "compile",
		Content:  map[string]any{"run": "true"},
	}

	err := pipeline.Store.RecordNode(context.Background(), record, "", "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode: %v", err)
	}

	_, body := get(t, server, "/p/demo/nodes/"+record.Hash)

	if strings.Contains(body, `href="/p/demo/jobs/"`) {
		t.Error("jobless node page renders an empty job link")
	}
}

// TestLiveNodeLinkMatchesServerSpelling: the live path built a RELATIVE node
// link while the server rendered an absolute one — two spellings of the same
// destination, and the relative one breaks the day the route gains a trailing
// segment. There is now one spelling because there is one renderer, and this
// is the test that says the stream really does use it.
func TestLiveNodeLinkMatchesServerSpelling(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-live", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-live", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "compile", StepKind: "task", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "compile", StepKind: "task", StepID: 1,
			Status: "succeeded", Hash: "cafe1234567"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-live", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	stream := sseHTML(streamOf(t, server, "/p/demo/runs/run-live/events"))
	if !strings.Contains(stream, `href="/p/demo/nodes/cafe1234567"`) {
		t.Errorf("the streamed row does not link its hash the way the page does:\n%s", stream)
	}
}

// TestJobPageExplainsReadOnly: a read-only server HIDES the trigger buttons;
// it has to say why, the way approvals already do.
func TestJobPageExplainsReadOnly(t *testing.T) {
	t.Parallel()

	// testPipeline passes runner == nil, which IS the read-only deployment.
	server, _ := testPipeline(t)

	_, body := get(t, server, "/p/demo/jobs/build")

	if !strings.Contains(body, "This server is read-only. Trigger with") {
		t.Error("read-only job page does not explain how to trigger from the CLI")
	}
}
