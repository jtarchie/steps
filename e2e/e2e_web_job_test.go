package e2e

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/web"
)

// TestJobLinkLandsOnItsLatestRun: every link that names a job resolves to
// the thing a reader came for. With nothing to show it falls through to the
// job's detail page; with a trigger queued it waits on the follow page for
// exactly that trigger's run; once a run exists it is the transcript.
func TestJobLinkLandsOnItsLatestRun(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t, happyPathScript()...)
	path := e2ePipeline(t, dir, fake.URL, "")

	server, pipeline := webServerFor(t, path)
	job := "/p/" + pipeline.Slug + "/jobs/build"

	expectRedirect(t, server, job, job+"/detail")
	expectPage(t, server, job+"/detail", "steps job build")

	err := pipeline.Store.EnqueueJob(t.Context(), "build", "test")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	queue, err := pipeline.Store.ListTriggerQueue(t.Context(), 1)
	if err != nil || len(queue) != 1 {
		t.Fatalf("ListTriggerQueue: %v (%d rows)", err, len(queue))
	}

	expectRedirect(t, server, job, job+"/follow?since="+strconv.FormatInt(queue[0].EnqueuedAt.UnixMilli(), 10))

	mustRun(t, "run", path, "--job", "build")

	runs, err := pipeline.Store.ListRuns(t.Context(), "build", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns: %v (%d rows)", err, len(runs))
	}

	run := "/p/" + pipeline.Slug + "/runs/" + runs[0].ID
	expectRedirect(t, server, job, run)

	transcript := expectPage(t, server, run, "steps run build")

	// The crumb is the one job link that does NOT resolve to a run: from inside
	// a run, "latest" is already on the page, and the crumb is how a reader
	// reaches what the transcript does not carry.
	if !strings.Contains(transcript, `href="`+job+`/detail">build</a>`) {
		t.Error("run page crumb does not lead to the job's detail page")
	}
}

// expectRedirect asks the server for target and fails unless it was sent to
// want with a 302 — a job's address must re-resolve on every visit, which a
// cacheable 301 would let a browser skip.
func expectRedirect(t *testing.T, server *web.Server, target, want string) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("GET %s = %d, want 302: %s", target, rec.Code, rec.Body.String())
	}

	if got := rec.Header().Get("Location"); got != want {
		t.Fatalf("GET %s → %q, want %q", target, got, want)
	}
}

func expectPage(t *testing.T, server *web.Server, target, marker string) string {
	t.Helper()

	code, body := webGet(t, server, target)
	if code != http.StatusOK || !strings.Contains(body, marker) {
		t.Fatalf("GET %s = %d, want 200 containing %q", target, code, marker)
	}

	return body
}
