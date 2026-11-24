package web

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// asked records what a page asked the poller to check.
type asked struct {
	mu    sync.Mutex
	names []string
	full  bool
}

func (a *asked) check(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.full {
		return false
	}

	a.names = append(a.names, name)

	return true
}

func wantPost(t *testing.T, server *Server, target string, want int, what string) {
	t.Helper()

	if code := post(t, server, target, nil); code != want {
		t.Errorf("%s = %d, want %d", what, code, want)
	}
}

// TestTheCheckButtonAsksThePoller: the button reaches the poller for a resource this pipeline declares, and each way it cannot is a refusal saying why rather than a redirect pretending it worked.
func TestTheCheckButtonAsksThePoller(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, body := get(t, server, "/p/demo/resources")
	if strings.Contains(body, `/resources/repo/check"`) {
		t.Error("a server nothing polls for offers a check")
	}

	wantPost(t, server, "/p/demo/resources/repo/check", http.StatusConflict, "checking with no poller")

	poller := &asked{}
	pipeline.CheckNow = poller.check

	_, body = get(t, server, "/p/demo/resources")
	if !strings.Contains(body, `action="/p/demo/resources/repo/check"`) {
		t.Error("the resources page offers no check")
	}

	wantPost(t, server, "/p/demo/resources/repo/check", http.StatusSeeOther, "checking")
	wantPost(t, server, "/p/demo/resources/nosuch/check", http.StatusNotFound, "checking an undeclared resource")

	poller.full = true
	wantPost(t, server, "/p/demo/resources/repo/check", http.StatusTooManyRequests, "checking past a full backlog")

	poller.full = false

	err = pipeline.Store.Pause(context.Background())
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	wantPost(t, server, "/p/demo/resources/repo/check", http.StatusConflict, "checking a paused pipeline")

	if len(poller.names) != 1 || poller.names[0] != "repo" {
		t.Errorf("the poller was asked for %v, want repo once", poller.names)
	}
}

// TestAVersionRunsAJobPinnedToIt: each version row offers each job a run against exactly that version, through the job's own trigger, and the box narrows the rows by what a version holds.
func TestAVersionRunsAJobPinnedToIt(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)
	runner := &pinRecorder{}

	server, err := New([]*Pipeline{pipeline}, runner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = pipeline.Store.RecordVersions(context.Background(), "repo", []map[string]any{{"ref": "v1"}, {"ref": "v2"}}, 0)
	if err != nil {
		t.Fatalf("RecordVersions: %v", err)
	}

	_, body := get(t, server, "/p/demo/resources/repo")
	if !strings.Contains(body, `<input type="hidden" name="pin.ref" value="v1">`) || !strings.Contains(body, `action="/p/demo/jobs/deploy/trigger"`) {
		t.Fatal("a version row offers no pinned run")
	}

	if code := post(t, server, "/p/demo/jobs/build/trigger", map[string]string{"pin.ref": "v1"}); code != http.StatusSeeOther {
		t.Fatalf("the pinned run = %d, want 303", code)
	}

	if len(runner.pins) != 1 || runner.pins[0] != "build trigger, pinned (web) map[ref:v1]" {
		t.Errorf("queued %v, want build pinned to ref v1", runner.pins)
	}

	_, narrowed := get(t, server, "/p/demo/resources/repo?q=V2")
	if strings.Contains(narrowed, `value="v1"`) || !strings.Contains(narrowed, `value="v2"`) {
		t.Error("the box does not narrow the versions")
	}
}

// TestAVersionsRunButtonIsAnActionNotAStatus: a bare ▶ beside a status read as part of that status, so the run is its own Action column and names the job it starts.
func TestAVersionsRunButtonIsAnActionNotAStatus(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)

	server, err := New([]*Pipeline{pipeline}, &pinRecorder{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = pipeline.Store.RecordVersions(context.Background(), "repo", []map[string]any{{"ref": "v1"}}, 0)
	if err != nil {
		t.Fatalf("RecordVersions: %v", err)
	}

	_, body := get(t, server, "/p/demo/resources/repo")
	if !strings.Contains(body, `<th scope="col" class="acts">Action</th>`) {
		t.Error("the versions table has no visible Action column")
	}

	if !strings.Contains(body, `>▶ Run deploy</button>`) {
		t.Error("the run button does not name the job it starts")
	}

	if strings.Contains(body, `class="inline cellrun"`) {
		t.Error("a run button still sits inside a job's status cell")
	}
}
