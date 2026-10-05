package web

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestResourceDetailShowsNeverCheckedEmptyState mirrors the collection page's
// own empty state (resources.html:18) — a resource nothing has checked yet
// should say so, not render a blank table.
func TestResourceDetailShowsNeverCheckedEmptyState(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, body := get(t, server, "/p/demo/resources/repo")

	if !strings.Contains(body, "never checked") {
		t.Errorf("unpolled resource's detail page does not say so:\n%s", body)
	}
}

// TestResourcesPageShowsNeverCheckedForAnUncheckedResource: index on a map of
// structs returns the zero value on a miss, not nil — a bare {{if $seen}}
// is therefore truthy even when nothing was found, and the collection page
// would silently render a blank "Latest version" cell instead of ever
// reaching its own "never checked" branch.
func TestResourcesPageShowsNeverCheckedForAnUncheckedResource(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, body := get(t, server, "/p/demo/resources")

	if !strings.Contains(body, "never checked") {
		t.Errorf("unchecked resource's row does not say so:\n%s", body)
	}
}

// TestResourceDetailListsVersionsNewestFirst: ResourceVersionsJSON returns
// oldest-first (the order a check discovered them in), which is the wrong
// order for a reader comparing against the "Latest version" column they just
// came from.
func TestResourceDetailListsVersionsNewestFirst(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	_, err := pipeline.Store.RecordVersions(ctx, "repo", []map[string]any{
		{"ref": "v1"}, {"ref": "v2"}, {"ref": "v3"},
	}, 0)
	if err != nil {
		t.Fatalf("RecordVersions: %v", err)
	}

	_, body := get(t, server, "/p/demo/resources/repo")

	first, second, third := strings.Index(body, "v3"), strings.Index(body, "v2"), strings.Index(body, "v1")
	if first < 0 || second < 0 || third < 0 {
		t.Fatalf("not all recorded versions rendered:\n%s", body)
	}

	if first >= second || second >= third {
		t.Errorf("versions did not render newest-first (v3, v2, v1): got offsets %d, %d, %d", first, second, third)
	}
}

// TestResourceDetailShowsWhatEachJobDidWithEachVersion is the page's reason to
// exist: a reader arrives asking "did my push build, and did it pass?", and
// a bare list of versions cannot say — a version never built looked exactly
// like one that passed.
func TestResourceDetailShowsWhatEachJobDidWithEachVersion(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	_, err := pipeline.Store.RecordVersions(ctx, "repo", []map[string]any{{"ref": "v1"}, {"ref": "v2"}}, 0)
	if err != nil {
		t.Fatalf("RecordVersions: %v", err)
	}

	recordRunOf(t, pipeline, "run-v2", "build", map[string]any{"ref": "v2"}, "succeeded")

	err = pipeline.Store.EnqueueJob(ctx, "deploy", "test")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	_, body := get(t, server, "/p/demo/resources/repo")

	for _, want := range []string{
		`<th scope="col"><a href="/p/demo/jobs/build">build</a></th>`,
		`<th scope="col"><a href="/p/demo/jobs/deploy">deploy</a></th>`,
		`href="/p/demo/runs/run-v2"`,
		`st st-passed`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("resource page missing %q", want)
		}
	}

	// v1 was never built by build, and v2 was: the older one was passed over
	// for the newer, which is a fact, not an empty cell.
	v1Row := rowOf(t, body, "v1")
	if !strings.Contains(v1Row, "superseded") {
		t.Errorf("v1's row does not say build passed it over:\n%s", v1Row)
	}

	// deploy has a pending queue row and has built nothing: the newest
	// version is what that row will build.
	v2Row := rowOf(t, body, "v2")
	if !strings.Contains(v2Row, `st-queued`) {
		t.Errorf("v2's row does not say deploy is queued for it:\n%s", v2Row)
	}
}

// rowOf is the table row a page draws for the version holding marker.
func rowOf(t *testing.T, body, marker string) string {
	t.Helper()

	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("page has no row for %s:\n%s", marker, body)
	}

	row, _, found := strings.Cut(body[start:], "</tr>")
	if !found {
		t.Fatalf("row for %s never closes", marker)
	}

	return row
}

// recordRunOf records a run of job that took version of repo, finished with
// status unless status is empty (a run still going).
func recordRunOf(t *testing.T, pipeline *Pipeline, runID, job string, version map[string]any, status string) {
	t.Helper()

	ctx := context.Background()

	encoded, err := store.EncodeVersion(version)
	if err != nil {
		t.Fatalf("EncodeVersion: %v", err)
	}

	err = pipeline.Store.StartRun(ctx, runID, job, "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = pipeline.Store.RecordRunInput(ctx, runID, "repo", "repo", encoded)
	if err != nil {
		t.Fatalf("RecordRunInput: %v", err)
	}

	if status == "" {
		return
	}

	err = pipeline.Store.FinishRun(ctx, runID, status)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
}

// resourceWithAVersion is testPipeline with one version of repo recorded.
func resourceWithAVersion(t *testing.T) (*Server, *Pipeline) {
	t.Helper()

	server, pipeline := testPipeline(t)

	_, err := pipeline.Store.RecordVersions(context.Background(), "repo", []map[string]any{{"ref": "abc123"}}, 0)
	if err != nil {
		t.Fatalf("RecordVersions: %v", err)
	}

	return server, pipeline
}
