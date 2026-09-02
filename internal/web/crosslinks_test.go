package web

import (
	"context"
	"strings"
	"testing"

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

// TestLiveNodeLinkMatchesServerSpelling: the SSE path built a RELATIVE node
// link while the server renders an absolute one — two spellings of the same
// destination, and the relative one breaks the day the route gains a
// trailing segment.
func TestLiveNodeLinkMatchesServerSpelling(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-live", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-live")

	if strings.Contains(body, "'../nodes/'") {
		t.Error("live script still builds a relative node link")
	}

	if !strings.Contains(body, "'/p/demo/nodes/'") {
		t.Error("live script does not build the absolute node link the server renders")
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
