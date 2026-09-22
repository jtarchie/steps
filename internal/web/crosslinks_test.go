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

// TestResourcesPageLinksItsVersionHistory: the collection page named a
// resource in plain text, making its recorded history unreachable — the same
// dead-end TestNodePageLinksItsJob caught for nodes.
func TestResourcesPageLinksItsVersionHistory(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	_, err := pipeline.Store.RecordVersions(ctx, "repo", []map[string]any{{"ref": "abc123"}}, 0)
	if err != nil {
		t.Fatalf("RecordVersions: %v", err)
	}

	_, listBody := get(t, server, "/p/demo/resources")
	if !strings.Contains(listBody, `<a href="/p/demo/resources/repo">repo</a>`) {
		t.Error("resources page does not link the resource's history")
	}

	code, detailBody := get(t, server, "/p/demo/resources/repo")
	if code != 200 {
		t.Fatalf("GET /p/demo/resources/repo = %d, want 200", code)
	}

	if !strings.Contains(detailBody, "abc123") {
		t.Errorf("resource detail page does not show its recorded version:\n%s", detailBody)
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

// A reader arrives at a broken mcp server from a RED RUN, not from the nav bar: the question "why are this agent's tools failing" is asked on the page where the failure is, and until this link existed the answer was on a tab you had to know about. The run page is also the only place that can carry it, since the job error is drawn there and nowhere the live stream reaches.
func TestARunThatFailedOnAnMCPServerLinksTheMCPTab(t *testing.T) {
	t.Parallel()

	server, _, _ := mcpServerPipeline(t, stubRunner{})
	pipeline := server.Lookup("demo")
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-mcp", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// The shape internal/mcp actually produces when a token is missing; the job's own error is what the page leads with.
	appendEvents(t, pipeline.Store, "run-mcp", []store.RunEventRow{{
		Type: events.TypeJobFinished,
		Text: `mcp server "linear" is not authorized (needs login linear, with -c <pipeline.yml> on this machine or -p <pipeline> --target <url> for a daemon)`,
	}})

	err = pipeline.Store.FinishRun(ctx, "run-mcp", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-mcp")

	if !strings.Contains(body, `href="/p/demo/mcp#mcp-linear"`) {
		t.Errorf("a run that died on an mcp server does not link that server's row:\n%s", body)
	}
}

// Every other red run must NOT grow the link: a task that exited 1 has nothing to do with mcp, and a page that offers the same suggestion for every failure is one nobody reads.
func TestAnOrdinaryFailureDoesNotLinkTheMCPTab(t *testing.T) {
	t.Parallel()

	server, _, _ := mcpServerPipeline(t, stubRunner{})
	pipeline := server.Lookup("demo")
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-plain", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-plain", []store.RunEventRow{{
		Type: events.TypeJobFinished,
		Text: "task compile: exit status 1",
	}})

	err = pipeline.Store.FinishRun(ctx, "run-plain", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-plain")

	if strings.Contains(body, "/p/demo/mcp#") {
		t.Errorf("an ordinary failure was decorated with an mcp link:\n%s", body)
	}
}

// The link has to land on the row it blames, not merely on the page: a pipeline with a dozen servers puts the one that failed anywhere on it.
func TestTheMCPTabGivesEachServerARowToLinkAt(t *testing.T) {
	t.Parallel()

	server, _, _ := mcpServerPipeline(t, stubRunner{})

	_, body := get(t, server, "/p/demo/mcp")
	if !strings.Contains(body, `id="mcp-linear"`) {
		t.Errorf("the mcp tab has no anchor for the row a failure names:\n%s", body)
	}
}

// A run kept long enough for its pipeline to stop declaring the server it died on must not end a triage at a 404: the tab is gone, and so is the link to it.
func TestAnMCPFailureOnAPipelineWithNoServersLinksNothing(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-gone", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-gone", []store.RunEventRow{{
		Type: events.TypeJobFinished,
		Text: `mcp server "linear" is not authorized`,
	}})

	err = pipeline.Store.FinishRun(ctx, "run-gone", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-gone")
	if strings.Contains(body, "/p/demo/mcp") {
		t.Errorf("a run linked an mcp tab its pipeline no longer has:\n%s", body)
	}
}
