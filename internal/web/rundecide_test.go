package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// parkTwoRuns leaves run-1 and run-2 each waiting on a question and an approval, and returns run-1's approval.
func parkTwoRuns(t *testing.T, pipeline *Pipeline) int64 {
	t.Helper()

	ctx := context.Background()

	askOne(t, pipeline)

	err := pipeline.Store.StartRun(ctx, store.NewRun{ID: "run-2", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	_, _, err = pipeline.Store.AskQuestion(ctx, store.Question{RunID: "run-2", JobName: "build", AgentName: "writer", Question: "Some other run's question?"})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	approval, err := pipeline.Store.RequestApprovalFor(ctx, "run-1", "step-a", "build", "Ship this build?")
	if err != nil {
		t.Fatalf("RequestApprovalFor: %v", err)
	}

	_, err = pipeline.Store.RequestApprovalFor(ctx, "run-2", "step-a", "build", "Ship the other build?")
	if err != nil {
		t.Fatalf("RequestApprovalFor: %v", err)
	}

	return approval
}

// TestARunPageAsksWhatItsRunWaitsOn: a person watching a run answers the question or approval it is parked on from that run's page (steps#228), and only that run's — another run's would be a decision about work they are not looking at.
func TestARunPageAsksWhatItsRunWaitsOn(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)
	parkTwoRuns(t, pipeline)

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	code, body := get(t, server, "/p/demo/runs/run-1")
	if code != http.StatusOK {
		t.Fatalf("GET run = %d", code)
	}

	waiting := markupBetween(t, body, `id="run-waiting"`, `class="transcripthead"`)

	for _, want := range []string{
		"major or a minor bump", `action="/p/demo/questions/1"`, `value="minor"`,
		"Ship this build?", `action="/p/demo/approvals/1"`,
	} {
		if !strings.Contains(waiting, want) {
			t.Errorf("run page's waiting region missing %q", want)
		}
	}

	if n := strings.Count(waiting, `name="return" value="/p/demo/runs/run-1"`); n != 2 {
		t.Errorf("%d forms send the person back to the run, want both the approval and the question", n)
	}

	for _, other := range []string{"Some other run", "Ship the other build"} {
		if strings.Contains(body, other) {
			t.Errorf("run page offers another run's decision: %q", other)
		}
	}

	if !strings.Contains(body, "#run-waiting:outerMorph") {
		t.Error("nothing polls #run-waiting, so an approval requested after the page opened never appears")
	}
}

// TestADecisionReturnsToThePageItWasMadeOn: deciding from a run page lands back on the run, a decided approval leaves it, and a return pointing off this server falls back to the tab.
func TestADecisionReturnsToThePageItWasMadeOn(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)
	approval := parkTwoRuns(t, pipeline)

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	code, where := postForm(t, server, "/p/demo/questions/1", map[string]string{"answer": "minor", "return": "/p/demo/runs/run-1"})
	if code != http.StatusSeeOther || where != "/p/demo/runs/run-1" {
		t.Errorf("answering from the run page = %d to %q, want back to the run", code, where)
	}

	code, where = postForm(t, server, "/p/demo/approvals/1", map[string]string{"decision": "approve", "return": "/p/demo/runs/run-1"})
	if code != http.StatusSeeOther || where != "/p/demo/runs/run-1" {
		t.Errorf("approving from the run page = %d to %q, want back to the run", code, where)
	}

	if decided, _ := pipeline.Store.ApprovalStatus(context.Background(), approval); decided.Status != store.ApprovalApproved {
		t.Errorf("approval = %q, want approved", decided.Status)
	}

	if _, body := get(t, server, "/p/demo/runs/run-1"); strings.Contains(body, "Ship this build?") {
		t.Error("a decided approval is still offered on the run page")
	}

	code, where = postForm(t, server, "/p/demo/approvals/2", map[string]string{"decision": "approve", "return": "//evil.example/"})
	if code != http.StatusSeeOther || where != "/p/demo/approvals" {
		t.Errorf("an off-site return = %d to %q, want the approvals tab", code, where)
	}
}

// markupBetween is the page from one marker to the next, to tell what is inside a region from what is merely on the page.
func markupBetween(t *testing.T, body, from, to string) string {
	t.Helper()

	start := strings.Index(body, from)
	end := strings.Index(body, to)

	if start < 0 || end < start {
		t.Fatalf("no %s before %s on the page", from, to)
	}

	return body[start:end]
}
