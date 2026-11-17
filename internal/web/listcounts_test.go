package web

import (
	"net/http"
	"strings"
	"testing"
)

// The runs page narrows by status as every other list does, and a chip narrows what a typed word found rather than adding to it.
func TestRunsNarrowByTheStatusChip(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	startFinishedRun(t, pipeline, "run-ok", "build", "succeeded")
	startFinishedRun(t, pipeline, "run-bad", "build", "failed")

	for _, tc := range []struct {
		query        string
		want, refuse []string
	}{
		{"", []string{"run-ok", "run-bad", `href="/p/demo/runs?status=failed"`}, nil},
		{"?status=failed", []string{"run-bad", "1 shown"}, []string{"run-ok"}},
		{"?status=passed", []string{"run-ok", "1 shown"}, []string{"run-bad"}},
		// "failed" typed matches run-bad's status word; the passed chip keeps only passed runs, so nothing is left.
		{"?status=passed&q=failed", []string{"No runs match."}, []string{"run-ok", "run-bad"}},
	} {
		code, body := get(t, server, "/p/demo/runs"+tc.query)
		if code != http.StatusOK {
			t.Fatalf("GET /p/demo/runs%s = %d", tc.query, code)
		}

		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Errorf("runs%s is missing %q", tc.query, want)
			}
		}

		for _, refuse := range tc.refuse {
			if strings.Contains(body, refuse) {
				t.Errorf("runs%s still shows %q", tc.query, refuse)
			}
		}
	}
}

// A narrowed list says how many rows it kept — "2 of 4" where it knows the whole, "N shown" where it is paged and cannot — and what the box searches is said under it, not in a placeholder a phone cuts off.
func TestANarrowedListSaysHowManyItKept(t *testing.T) {
	t.Parallel()

	server, _, _ := mcpServerPipeline(t, stubRunner{})

	_, body := get(t, server, "/p/demo/mcp?transport=stdio")
	if !strings.Contains(body, "2 of 4") {
		t.Errorf("a narrowed mcp tab does not say it kept 2 of 4:\n%s", body)
	}

	_, body = get(t, server, "/p/demo/mcp?q=nothing-is-called-this")
	if !strings.Contains(body, "0 of 4") {
		t.Errorf("an mcp search that kept nothing does not say 0 of 4:\n%s", body)
	}

	plain, _ := testPipeline(t)

	_, body = get(t, plain, "/p/demo/resources?type=mock")
	if !strings.Contains(body, "1 of 1") {
		t.Errorf("a narrowed resources page does not say 1 of 1:\n%s", body)
	}

	_, body = get(t, plain, "/p/demo/runs")

	for _, want := range []string{`id="list-hint"`, `aria-describedby="list-hint"`, "run id, status, or what it ran against"} {
		if !strings.Contains(body, want) {
			t.Errorf("the runs search box is missing %q", want)
		}
	}

	if strings.Contains(body, `placeholder="run id`) {
		t.Error("the search hint is back in the placeholder, where a phone cuts it off")
	}
}
