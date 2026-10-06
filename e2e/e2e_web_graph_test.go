package e2e

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// graphPipeline is the smallest pipeline that exercises every edge the jobs
// graph draws: a triggering input, an output, a passed: constraint that runs
// THROUGH that output into the next job, and an input that only reads.
const graphPipeline = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: shell
  config:
    check: echo '[{"v":"1"}]'
    in: echo ok > v.txt
    out: echo '{"v":"2"}'
resources:
- name: src
  type: shell
  source: {}
- name: out
  type: shell
  source: {}
- name: docs
  type: shell
  source: {}
jobs:
- name: a
  plan:
  - get: src
    trigger: true
  - put: out
- name: b
  plan:
  - get: out
    passed: [a]
    trigger: true
  - get: docs
  - task: work
    run: "true"
`

// TestJobsGraphDrawsResourcesTheWayConcourseDoes: the jobs page draws a
// pipeline as Concourse does — resources are nodes, a job's gets on its left
// and puts on its right, a passed: constraint arriving from the upstream
// job's output node, and an input that does not trigger drawn as one that
// only reads. Before this the graph drew jobs alone, so a pipeline with no
// passed: was a column of unconnected boxes.
func TestJobsGraphDrawsResourcesTheWayConcourseDoes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "graph.yml")
	writePipelineFile(t, path, graphPipeline)

	mustRun(t, "run", path, "--job", "a")

	server, pipeline := webServerFor(t, path)

	code, body := webGet(t, server, "/p/"+pipeline.Slug)
	if code != 200 {
		t.Fatalf("GET /p/%s = %d", pipeline.Slug, code)
	}

	_, svg, found := strings.Cut(body, `<svg class="dag"`)
	if !found {
		t.Fatal("jobs page has no graph")
	}

	svg, _, _ = strings.Cut(svg, "</svg>")

	for _, want := range []struct{ fragment, cost string }{
		{`data-n="in:a:src"`, "a get with no passed: is not its own resource node"},
		{`data-n="out:a:out"`, "a put is not an output node on its job"},
		{`data-n="in:b:docs"`, "an input that only reads is not drawn"},
		{`href="/p/` + pipeline.Slug + `/resources/src"`, "a resource node does not lead to its versions"},
	} {
		if !strings.Contains(svg, want.fragment) {
			t.Errorf("graph lacks %s: %s", want.fragment, want.cost)
		}
	}

	edges := graphEdges(svg)

	for _, want := range []struct {
		from, to string
		manual   bool
		cost     string
	}{
		{"in:a:src", "job:a", false, "the trigger into a is missing or drawn as read-only"},
		{"job:a", "out:a:out", false, "a does not reach its output"},
		{"out:a:out", "job:b", false, "the passed: constraint does not run through a's output into b"},
		{"in:b:docs", "job:b", true, "an input that does not trigger is drawn as one that does"},
	} {
		manual, ok := edges[want.from+">"+want.to]
		if !ok || manual != want.manual {
			t.Errorf("edge %s → %s (manual=%v): %s; edges: %v", want.from, want.to, want.manual, want.cost, edges)
		}
	}

	if !regexp.MustCompile(`class="dagnode st-passed[^"]*"[^>]*data-n="job:a"`).MatchString(svg) {
		t.Error("job a ran green but its node does not say so")
	}
}

// graphEdges reads every drawn edge as "from>to" → whether it is dashed.
func graphEdges(svg string) map[string]bool {
	edges := map[string]bool{}

	for _, match := range regexp.MustCompile(`<path [^>]*class="dagedge( manual)?"[^>]*data-from="([^"]+)" data-to="([^"]+)"`).FindAllStringSubmatch(svg, -1) {
		edges[match[2]+">"+match[3]] = match[1] != ""
	}

	return edges
}

// TestJobsPageIsTheGraphAndItRefreshes: the list view is gone, so the graph
// is what polls — a run landing while the page is open shows on its node at
// the next refresh, with no reload and no view to pick first.
func TestJobsPageIsTheGraphAndItRefreshes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "graph.yml")
	writePipelineFile(t, path, graphPipeline)

	server, pipeline := webServerFor(t, path)
	page := "/p/" + pipeline.Slug

	_, before := webGet(t, server, page)

	for _, gone := range []string{`id="jobs-list"`, `class="viewtoggle"`} {
		if strings.Contains(before, gone) {
			t.Errorf("the jobs page still draws %s", gone)
		}
	}

	region := polledRegion(t, before, "jobs-graph")
	if !strings.Contains(region, `data-n="job:a"`) || !strings.Contains(region, "never ran") {
		t.Fatalf("job a is not in the polled graph, or claims a run it never had:\n%s", region)
	}

	mustRun(t, "run", path, "--job", "a")

	_, after := webGet(t, server, page)
	if region := polledRegion(t, after, "jobs-graph"); !strings.Contains(region, "✓ passed") {
		t.Errorf("a's run does not reach the polled graph:\n%s", region)
	}
}

// polledRegion is the element with the given id, from its opening tag to the
// end of its svg, failing unless it is the element that polls and selects
// itself — the shape every live page's region has.
func polledRegion(t *testing.T, body, id string) string {
	t.Helper()

	open := regexp.MustCompile(`<[a-z]+ [^>]*id="` + id + `"[^>]*>`).FindString(body)
	for _, want := range []string{`hx-trigger="every[!document.hidden] 2500ms"`, `hx-select="#` + id + `"`} {
		if !strings.Contains(open, want) {
			t.Fatalf("#%s does not poll (%s missing): %s", id, want, open)
		}
	}

	_, region, _ := strings.Cut(body, open)
	region, _, _ = strings.Cut(region, "</svg>")

	return region
}
