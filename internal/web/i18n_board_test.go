package web

import (
	"net/http"
	"regexp"
	"slices"
	"testing"
)

var svgRe = regexp.MustCompile(`(?s)<svg\b[^>]*>(.*?)</svg>`)

// graphWords is the words the page-wide check cannot see, because it skips <svg>: the node lines, titles and labels.
func graphWords(page, fixture string) []string {
	left := make([]string, 0, 8)

	for _, graph := range svgRe.FindAllStringSubmatch(page, -1) {
		left = append(left, untranslated("<body>"+graph[1], fixture)...)
	}

	return left
}

// TestTheBoardAndOverviewSpeakOnlyThroughTheCatalog: the jobs board and its graph (with a held job, a queued run and a run in the strip), the job page and the multi-pipeline overview, in the pseudo-locale.
func TestTheBoardAndOverviewSpeakOnlyThroughTheCatalog(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	server.pseudo = true

	seedPseudoFixtures(t, pipeline)

	_, _, err := pipeline.Store.RecordJobOutcome(t.Context(), "build", false, 1)
	if err != nil {
		t.Fatalf("RecordJobOutcome: %v", err)
	}

	err = pipeline.Store.EnqueueJob(t.Context(), "deploy", "new version", "")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	for _, path := range []string{"/p/demo", "/p/demo/jobs/build/detail", "/p/demo/jobs/deploy/detail"} {
		_, page := getAs(t, server, path, http.Header{"Accept-Language": {pseudoTag}})

		if left := append(untranslated(page, t.Name()), graphWords(page, t.Name())...); len(left) > 0 {
			t.Errorf("%s: plain English outside the catalog: %v", path, slices.Compact(slices.Sorted(slices.Values(left))))
		}
	}
}

func TestTheOverviewSpeaksOnlyThroughTheCatalog(t *testing.T) {
	t.Parallel()

	server, pipelines := testPipelines(t, "app", "api")
	server.pseudo = true

	err := pipelines[0].Store.EnqueueJob(t.Context(), "app-job", "new version", "")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	_, _, err = pipelines[1].Store.RecordJobOutcome(t.Context(), "api-job", false, 1)
	if err != nil {
		t.Fatalf("RecordJobOutcome: %v", err)
	}

	server.MarkBroken(BrokenPipeline{Name: "gone", Reason: "workspace: root is not writable"})

	_, page := getAs(t, server, "/", http.Header{"Accept-Language": {pseudoTag}})

	if left := append(untranslated(page, t.Name()+" api app gone job"), graphWords(page, t.Name()+" api app gone job")...); len(left) > 0 {
		t.Errorf("/: plain English outside the catalog: %v", slices.Compact(slices.Sorted(slices.Values(left))))
	}
}
