package web

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

func trigger(resource string, passed ...string) config.JobInput {
	return config.JobInput{Resource: resource, Trigger: true, Passed: passed}
}

func reads(resource string, passed ...string) config.JobInput {
	return config.JobInput{Resource: resource, Passed: passed}
}

func graphJobs(graph graphView) map[string]graphNode {
	jobs := map[string]graphNode{}

	for _, node := range graph.Nodes {
		if node.Kind == "job" {
			jobs[node.Name] = node
		}
	}

	return jobs
}

func graphNodeIDs(graph graphView) map[string]graphNode {
	ids := map[string]graphNode{}
	for _, node := range graph.Nodes {
		ids[node.ID] = node
	}

	return ids
}

func graphEdgeSet(graph graphView) map[string]graphEdge {
	edges := map[string]graphEdge{}
	for _, edge := range graph.Edges {
		edges[edge.From+">"+edge.To] = edge
	}

	return edges
}

// TestGraphLayersFollowDependencies: a diamond of passed: constraints lays
// out left to right — a job sits right of everything it waits on, and two
// jobs waiting on the same thing share a column.
func TestGraphLayersFollowDependencies(t *testing.T) {
	t.Parallel()

	jobs := graphJobs(graphOf([]jobView{
		{Name: "a", Inputs: []config.JobInput{trigger("repo")}},
		{Name: "b", Inputs: []config.JobInput{trigger("repo", "a")}},
		{Name: "c", Inputs: []config.JobInput{trigger("repo", "a")}},
		{Name: "d", Inputs: []config.JobInput{trigger("repo", "b", "c")}},
	}))

	a, b, c, d := jobs["a"].Layer, jobs["b"].Layer, jobs["c"].Layer, jobs["d"].Layer
	if a >= b || b != c || c >= d {
		t.Errorf("layers a=%d b=%d c=%d d=%d, want a < b = c < d", a, b, c, d)
	}
}

// TestGraphDoesNotJoinAPutToAGetWithoutPassed: Concourse's rule, and the
// promise an edge makes. b getting what a puts does not make b wait for a, so
// the two are drawn apart: a's output on a's right, b's input on b's left.
func TestGraphDoesNotJoinAPutToAGetWithoutPassed(t *testing.T) {
	t.Parallel()

	graph := graphOf([]jobView{
		{Name: "a", Outputs: []string{"x"}},
		{Name: "b", Inputs: []config.JobInput{trigger("x")}},
	})

	ids, edges := graphNodeIDs(graph), graphEdgeSet(graph)

	for _, id := range []string{"out:a:x", "in:b:x"} {
		if ids[id].Kind != "resource" {
			t.Errorf("no resource node %s", id)
		}
	}

	if len(edges) != 2 || edges["job:a>out:a:x"].Path == "" || edges["in:b:x>job:b"].Path == "" {
		t.Errorf("edges %v, want exactly a → its output and b's input → b", edges)
	}
}

// TestGraphRoutesPassedThroughTheUpstreamOutput: when the upstream job puts
// the resource, the constraint leaves from that output node, so the drawing
// reads "a made it, b takes it".
func TestGraphRoutesPassedThroughTheUpstreamOutput(t *testing.T) {
	t.Parallel()

	edges := graphEdgeSet(graphOf([]jobView{
		{Name: "a", Outputs: []string{"x"}},
		{Name: "b", Inputs: []config.JobInput{trigger("x", "a")}},
	}))

	if edges["out:a:x>job:b"].Path == "" {
		t.Errorf("passed: does not run through a's output: %v", edges)
	}

	if _, ok := edges["in:b:x>job:b"]; ok {
		t.Error("a constrained input also drew an unconstrained one")
	}
}

// TestGraphRoutesPassedThroughItsOwnNodeWhenUpstreamOnlyGets: an upstream
// that gets the resource without putting it has no output node, so the
// constraint gets a node of its own — Concourse's constrained-input.
func TestGraphRoutesPassedThroughItsOwnNodeWhenUpstreamOnlyGets(t *testing.T) {
	t.Parallel()

	edges := graphEdgeSet(graphOf([]jobView{
		{Name: "a", Inputs: []config.JobInput{trigger("repo")}},
		{Name: "b", Inputs: []config.JobInput{trigger("repo", "a")}},
	}))

	for _, want := range []string{"in:a:repo>job:a", "job:a>via:a:repo", "via:a:repo>job:b"} {
		if edges[want].Path == "" {
			t.Errorf("missing edge %s: %v", want, edges)
		}
	}
}

// TestGraphMergesAnInputSharedWithinAColumn: two jobs side by side reading
// one resource share its node; the same input further right gets its own, so
// no edge crosses the drawing to reach it.
func TestGraphMergesAnInputSharedWithinAColumn(t *testing.T) {
	t.Parallel()

	resources := func(graph graphView, name string) int {
		n := 0

		for _, node := range graph.Nodes {
			if node.Kind == "resource" && node.Name == name {
				n++
			}
		}

		return n
	}

	sameColumn := graphOf([]jobView{
		{Name: "a", Inputs: []config.JobInput{trigger("src")}},
		{Name: "b", Inputs: []config.JobInput{reads("src")}},
	})
	if n := resources(sameColumn, "src"); n != 1 {
		t.Errorf("one column: %d src nodes, want 1", n)
	}

	if len(sameColumn.Edges) != 2 {
		t.Errorf("one column: %d edges, want one from the shared node to each job", len(sameColumn.Edges))
	}

	twoColumns := graphOf([]jobView{
		{Name: "a", Inputs: []config.JobInput{trigger("src")}, Outputs: []string{"out"}},
		{Name: "b", Inputs: []config.JobInput{trigger("out", "a"), reads("src")}},
	})
	if n := resources(twoColumns, "src"); n != 2 {
		t.Errorf("two columns: %d src nodes, want one beside each job", n)
	}
}

// TestGraphSpacesAnEdgeThatSpansColumns: an edge never crosses a column it
// does not belong to; it runs through ghost copies of its resource instead.
func TestGraphSpacesAnEdgeThatSpansColumns(t *testing.T) {
	t.Parallel()

	graph := graphOf([]jobView{
		{Name: "a", Outputs: []string{"x"}},
		{Name: "b", Inputs: []config.JobInput{trigger("x", "a")}, Outputs: []string{"y"}},
		{Name: "c", Inputs: []config.JobInput{trigger("y", "b"), reads("x", "a")}},
	})

	ids := graphNodeIDs(graph)

	spacers := 0

	for _, node := range graph.Nodes {
		if node.Kind == "spacer" {
			spacers++

			if node.Name != "x" {
				t.Errorf("spacer stands for %q, want the resource it carries", node.Name)
			}
		}
	}

	if spacers != 2 {
		t.Errorf("%d spacers, want 2 for x's edge from a's output to c", spacers)
	}

	for _, edge := range graph.Edges {
		if span := ids[edge.To].Layer - ids[edge.From].Layer; span != 1 {
			t.Errorf("edge %s → %s spans %d columns", edge.From, edge.To, span)
		}
	}
}

// TestGraphKeepsAJobAndAResourceOfOneNameApart: shipped-update has a job and
// a resource both called draft; one node apiece, joined by the put.
func TestGraphKeepsAJobAndAResourceOfOneNameApart(t *testing.T) {
	t.Parallel()

	graph := graphOf([]jobView{{Name: "draft", Outputs: []string{"draft"}}})

	ids := graphNodeIDs(graph)
	if ids["job:draft"].Kind != "job" || ids["out:draft:draft"].Kind != "resource" {
		t.Errorf("nodes %v, want a job and a resource", ids)
	}

	if graphEdgeSet(graph)["job:draft>out:draft:draft"].Path == "" {
		t.Error("the job does not reach its own-named output")
	}
}

// TestGraphMarksOnlyNonTriggeringInputsManual: dashed is the second channel
// for "this never starts the job", so a trigger must never wear it — and an
// output, which triggers nothing, must not either.
func TestGraphMarksOnlyNonTriggeringInputsManual(t *testing.T) {
	t.Parallel()

	edges := graphEdgeSet(graphOf([]jobView{
		{Name: "a", Inputs: []config.JobInput{trigger("src"), reads("docs")}, Outputs: []string{"out"}},
		{Name: "b", Inputs: []config.JobInput{reads("out", "a")}},
	}))

	for key, manual := range map[string]bool{
		"in:a:src>job:a":  false,
		"in:a:docs>job:a": true,
		"job:a>out:a:out": false,
		"out:a:out>job:b": true,
	} {
		edge, ok := edges[key]
		if !ok || edge.Manual != manual {
			t.Errorf("%s: manual=%v (drawn: %v), want %v", key, edge.Manual, ok, manual)
		}
	}
}

// TestGraphGivesEachResourceItsOwnPort: a job with several inputs is tall
// enough that each lands at its own point, the way Concourse's do, rather
// than every edge converging on one.
func TestGraphGivesEachResourceItsOwnPort(t *testing.T) {
	t.Parallel()

	graph := graphOf([]jobView{
		{Name: "a", Inputs: []config.JobInput{trigger("one"), trigger("two"), reads("three")}},
	})

	if h := graphJobs(graph)["a"].H; h <= dagNodeH {
		t.Errorf("job with three inputs is %dpx tall, no taller than one with none", h)
	}

	ends := map[string]bool{}

	for _, edge := range graph.Edges {
		end := regexp.MustCompile(`\d+,\d+$`).FindString(edge.Path)
		if ends[end] {
			t.Errorf("two edges meet the job at %s", end)
		}

		ends[end] = true
	}
}

// TestGraphSurvivesACycle: a cycle in passed: constraints must not hang the
// layout — every job still gets a node.
func TestGraphSurvivesACycle(t *testing.T) {
	t.Parallel()

	graph := graphOf([]jobView{
		{Name: "a", Inputs: []config.JobInput{trigger("r", "b")}},
		{Name: "b", Inputs: []config.JobInput{trigger("r", "a")}},
	})

	if len(graphJobs(graph)) != 2 {
		t.Errorf("jobs: %d, want 2", len(graphJobs(graph)))
	}
}

// TestGraphNodesCarryStatusWord: the ASCII DAG this replaces conveyed status
// by a colored dot alone; the SVG node says the word, one glyph per outcome.
func TestGraphNodesCarryStatusWord(t *testing.T) {
	t.Parallel()

	views := []jobView{
		{Name: "a", HasRun: true, Latest: store.RunRow{Status: "succeeded"}},
		{Name: "b"},
		{Name: "c", HasRun: true, Latest: store.RunRow{Status: "failed"}},
		{Name: "d", HasRun: true, Latest: store.RunRow{Status: ""}},
		{Name: "e", HasRun: true, Latest: store.RunRow{Status: "pending"}},
	}

	byName := graphJobs(graphOf(views))

	want := map[string][2]string{
		"a": {"passed", "✓"},
		"b": {"never ran", "○"},
		"c": {"failed", "✗"},
		"d": {"running", "◐"},
		"e": {"queued", "○"},
	}

	for name, expect := range want {
		if byName[name].Status != expect[0] || byName[name].Glyph != expect[1] {
			t.Errorf("%s: got %q %q, want %q %q",
				name, byName[name].Glyph, byName[name].Status, expect[1], expect[0])
		}
	}
}

// TestGraphIgnoresAnUnknownUpstream: a passed: naming a job the pipeline does
// not define draws no edge and breaks no layout.
func TestGraphIgnoresAnUnknownUpstream(t *testing.T) {
	t.Parallel()

	graph := graphOf([]jobView{
		{Name: "orphan-dependent", Inputs: []config.JobInput{trigger("r", "no-such-job")}},
	})

	if len(graph.Nodes) != 1 {
		t.Fatalf("nodes: %d, want the job alone", len(graph.Nodes))
	}

	if len(graph.Edges) != 0 {
		t.Errorf("edges: %d, want 0 for an unknown upstream", len(graph.Edges))
	}
}

// TestGraphNodeGrowsWithItsName: a long job name widens its node instead of
// overflowing it.
func TestGraphNodeGrowsWithItsName(t *testing.T) {
	t.Parallel()

	// Two graphs, because within one graph a column's nodes share its width.
	short := graphOf([]jobView{{Name: "a"}}).Nodes[0].W
	long := graphOf([]jobView{{Name: "a-job-with-a-deliberately-long-name"}}).Nodes[0].W

	if long <= short {
		t.Errorf("long-named node (%dpx) is not wider than the short one (%dpx)", long, short)
	}
}

// TestJobsBoardGraphIsSVG: the board renders real nodes that link to job
// and resource pages, not ASCII art.
func TestJobsBoardGraphIsSVG(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	code, body := get(t, server, "/p/demo")
	if code != http.StatusOK {
		t.Fatalf("GET /p/demo = %d", code)
	}

	_, tail, found := strings.Cut(body, `<svg class="dag"`)
	if !found {
		t.Fatal("jobs board has no SVG graph")
	}

	svg, _, found := strings.Cut(tail, "</svg>")
	if !found {
		t.Fatal("graph svg never closes")
	}

	for _, want := range []string{
		`class="dagnode`,
		`class="dagedge`,
		`class="dagres"`,
		`href="/p/demo/jobs/build"`,
		`href="/p/demo/jobs/deploy"`,
		`href="/p/demo/resources/repo"`,
		"never ran",
		`aria-label="build, never ran, reads repo"`,
		`aria-label="resource repo"`,
		`<path aria-hidden="true" class="dagedge`,
		// role="img" would flatten the job links and status text out of the
		// accessibility tree; a group keeps them reachable.
		`role="group"`,
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("graph svg missing %q", want)
		}
	}
}

// TestJobsBoardDrawsAJobByItsLastFinishedRun: Concourse's dashboard rule at
// node size. A red job being rebuilt stays red and wears the running ring,
// rather than reading as merely busy; its line says what it is doing now.
func TestJobsBoardDrawsAJobByItsLastFinishedRun(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	seedRun(t, pipeline, "b1", "build", "failed")
	seedRun(t, pipeline, "b2", "build", "")
	seedRun(t, pipeline, "d1", "deploy", "succeeded")

	_, body := get(t, server, "/p/demo")

	svg := dagSVG(t, body)

	build := dagNodeMarkup(t, svg, "job:build")
	if !strings.Contains(build, `class="dagnode st-failed running"`) {
		t.Errorf("a failed job rebuilding is not drawn red with the ring: %s", build)
	}

	if !strings.Contains(build, `class="dagring"`) || !strings.Contains(build, "◐ running") {
		t.Errorf("the rebuilding job has no ring or does not say it is running: %s", build)
	}

	deploy := dagNodeMarkup(t, svg, "job:deploy")
	if !strings.Contains(deploy, `class="dagnode st-passed"`) || !strings.Contains(deploy, "✓ passed") {
		t.Errorf("a passed job is not drawn passed: %s", deploy)
	}

	if strings.Contains(deploy, "dagring") {
		t.Error("a job with nothing running wears the ring")
	}
}

// seedRun starts a run of job and, unless status is empty, finishes it so.
func seedRun(t *testing.T, pipeline *Pipeline, id, job, status string) {
	t.Helper()

	err := pipeline.Store.StartRun(t.Context(), id, job, "/tmp/ws", "")
	if err != nil {
		t.Fatal(err)
	}

	if status != "" {
		err = pipeline.Store.FinishRun(t.Context(), id, status)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func dagSVG(t *testing.T, body string) string {
	t.Helper()

	_, tail, found := strings.Cut(body, `<svg class="dag"`)
	if !found {
		t.Fatal("page has no graph")
	}

	svg, _, _ := strings.Cut(tail, "</svg>")

	return svg
}

// dagNodeMarkup is one job node's anchor, opening tag to close.
func dagNodeMarkup(t *testing.T, svg, id string) string {
	t.Helper()

	match := regexp.MustCompile(`<a class="dagnode[^"]*"[^>]*data-n="` + regexp.QuoteMeta(id) + `"[\s\S]*?</a>`).FindString(svg)
	if match == "" {
		t.Fatalf("no node %s in %s", id, svg)
	}

	return match
}

// TestGraphNodesNameThemselvesForAScreenReader: the list view was the board's
// accessible form, and it is gone, so each node says in words what the
// drawing says in lines — its status, what triggers it, what it only reads,
// what it waits on upstream, and what it writes.
func TestGraphNodesNameThemselvesForAScreenReader(t *testing.T) {
	t.Parallel()

	started := time.Now().Add(-15 * time.Hour)
	graph := graphOf([]jobView{
		{
			Name: "a", HasRun: true, Latest: store.RunRow{Status: "succeeded", StartedAt: started},
			Inputs:  []config.JobInput{trigger("src"), reads("docs")},
			Outputs: []string{"x", "y"},
		},
		{
			Name: "b", HasRun: true, Latest: store.RunRow{Status: "running", StartedAt: started},
			Finished: store.RunRow{Status: "failed"}, HasFinished: true,
			Held: true, Queued: &queuedJob{WaitingOn: "the pipeline is paused"},
			Inputs: []config.JobInput{trigger("x", "a")},
		},
		{Name: "c"},
	})

	ids := graphNodeIDs(graph)

	for id, want := range map[string]string{
		"job:a":    "a, passed 15 hours ago, triggered by src, reads docs, writes x and y",
		"job:b":    "b, running, started 15 hours ago, last finished failed, held, queued, waiting: the pipeline is paused, triggered by x once it has passed a",
		"job:c":    "c, never ran",
		"in:a:src": "resource src",
	} {
		if got := ids[id].Label; got != want {
			t.Errorf("%s is named\n  %q\nwant\n  %q", id, got, want)
		}
	}
}

// TestGraphNodesComeInReadingOrder: a keyboard tabs through the job and
// resource links in DOM order, so the DOM runs column by column, top to
// bottom, the way the drawing reads.
func TestGraphNodesComeInReadingOrder(t *testing.T) {
	t.Parallel()

	graph := graphOf([]jobView{
		{Name: "z-last-in-config", Inputs: []config.JobInput{trigger("out", "a")}},
		{Name: "a", Inputs: []config.JobInput{trigger("src"), reads("docs")}, Outputs: []string{"out"}},
		{Name: "m", Inputs: []config.JobInput{trigger("src")}},
	})

	for i := 1; i < len(graph.Nodes); i++ {
		prev, node := graph.Nodes[i-1], graph.Nodes[i]
		if node.Layer < prev.Layer || (node.Layer == prev.Layer && node.Y < prev.Y) {
			t.Errorf("%s (column %d, y %d) comes after %s (column %d, y %d)",
				node.ID, node.Layer, node.Y, prev.ID, prev.Layer, prev.Y)
		}
	}
}

// graphOf lays out views with no resource failing its check.
func graphOf(views []jobView) graphView {
	return buildGraph(views, nil)
}

// TestGraphMarksAResourceWhoseCheckFails: a resource whose last check failed
// says so where it is drawn — the first line of why, a ! a reader can see
// without hovering, and the words a screen reader says — and its box grows
// to hold the mark rather than clipping its name.
func TestGraphMarksAResourceWhoseCheckFails(t *testing.T) {
	t.Parallel()

	views := []jobView{{Name: "a", Inputs: []config.JobInput{trigger("src"), reads("docs")}}}

	healthy := graphNodeIDs(graphOf(views))
	failing := graphNodeIDs(buildGraph(views, map[string]store.CheckError{
		"src": {Name: "src", Message: "exit status 1: no such repo\nfull stderr follows"},
	}))

	src := failing["in:a:src"]
	if src.CheckError != "exit status 1: no such repo" || src.Label != "resource src, check failing" {
		t.Errorf("failing src: error %q, label %q", src.CheckError, src.Label)
	}

	if src.W <= healthy["in:a:src"].W {
		t.Errorf("failing src is %dpx wide, no wider than healthy (%dpx): the ! clips its name", src.W, healthy["in:a:src"].W)
	}

	if docs := failing["in:a:docs"]; docs.CheckError != "" || docs.Label != "resource docs" {
		t.Errorf("docs checks fine but reads %q / %q", docs.CheckError, docs.Label)
	}
}

// TestJobsBoardShowsAFailingCheckUntilItRecovers: the page reads the store's
// check errors on every render, so a recovered check clears its mark at the
// next poll.
func TestJobsBoardShowsAFailingCheckUntilItRecovers(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	err := pipeline.Store.RecordCheckError(t.Context(), "repo", "exit status 1: nope")
	if err != nil {
		t.Fatal(err)
	}

	_, body := get(t, server, "/p/demo")
	if !regexp.MustCompile(`<a class="dagres check-failed"[^>]*aria-label="resource repo, check failing">\s*<title>exit status 1: nope</title>`).MatchString(dagSVG(t, body)) {
		t.Errorf("repo's failing check is not on its node:\n%s", dagSVG(t, body))
	}

	err = pipeline.Store.RecordCheckError(t.Context(), "repo", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, body = get(t, server, "/p/demo"); strings.Contains(body, "check-failed") {
		t.Error("repo recovered but its node still says the check fails")
	}
}

// TestGraphStartsAtItsTopMargin: aligning nodes with their neighbours only
// ever moves them down, so without a final lift the whole drawing sat below
// an empty band at the top of its pane.
func TestGraphStartsAtItsTopMargin(t *testing.T) {
	t.Parallel()

	graph := graphOf([]jobView{
		{Name: "unit", Inputs: []config.JobInput{trigger("src")}},
		{Name: "lint", Inputs: []config.JobInput{trigger("src"), reads("docs")}},
		{Name: "build", Inputs: []config.JobInput{trigger("src", "unit", "lint")}, Outputs: []string{"image"}},
		{Name: "deploy", Inputs: []config.JobInput{reads("image", "build"), reads("src", "build")}},
	})

	top := graph.H
	for _, node := range graph.Nodes {
		top = min(top, node.Y)
	}

	if top != dagMargin {
		t.Errorf("the highest node sits at y=%d, want the %dpx margin", top, dagMargin)
	}
}
