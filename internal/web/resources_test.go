package web

import (
	"context"
	"strings"
	"testing"
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

// TestResourceDetailListsVersionsNewestFirst: ResourceVersions returns
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
