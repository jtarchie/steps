package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

func graphFixture() []jobView {
	views := []jobView{
		{Name: "a"},
		{Name: "b", Upstream: []edgeView{{Resource: "repo", Job: "a"}}},
		{Name: "c", Upstream: []edgeView{{Resource: "repo", Job: "a"}}},
		{Name: "d", Upstream: []edgeView{{Resource: "repo", Job: "b"}, {Resource: "img", Job: "c"}}},
	}

	return linkDownstream(views)
}

// TestGraphLayersFollowDependencies: a diamond lays out left to right — a
// job sits one column past the deepest thing it waits on.
func TestGraphLayersFollowDependencies(t *testing.T) {
	t.Parallel()

	graph := buildGraph(graphFixture())

	layers := map[string]int{}
	for _, node := range graph.Nodes {
		layers[node.Name] = node.Layer
	}

	want := map[string]int{"a": 0, "b": 1, "c": 1, "d": 2}
	for name, layer := range want {
		if layers[name] != layer {
			t.Errorf("job %s: layer %d, want %d", name, layers[name], layer)
		}
	}
}

// TestGraphDrawsEveryJobOnceAndEveryConstraintOnce: nodes are jobs, edges are
// passed: constraints — no more, no less.
func TestGraphDrawsEveryJobOnceAndEveryConstraintOnce(t *testing.T) {
	t.Parallel()

	graph := buildGraph(graphFixture())

	if len(graph.Nodes) != 4 {
		t.Errorf("nodes: %d, want 4", len(graph.Nodes))
	}

	if len(graph.Edges) != 4 {
		t.Errorf("edges: %d, want 4", len(graph.Edges))
	}

	resources := map[string]bool{}
	for _, edge := range graph.Edges {
		resources[edge.Resource] = true

		if edge.Path == "" {
			t.Errorf("edge %s→%s has no path", edge.From, edge.To)
		}
	}

	if !resources["repo"] || !resources["img"] {
		t.Errorf("edges lost their resource identity: %v", resources)
	}
}

// TestGraphSurvivesACycle: a cycle in passed: constraints must not hang the
// layout — every job still gets a node.
func TestGraphSurvivesACycle(t *testing.T) {
	t.Parallel()

	graph := buildGraph(linkDownstream([]jobView{
		{Name: "a", Upstream: []edgeView{{Resource: "r", Job: "b"}}},
		{Name: "b", Upstream: []edgeView{{Resource: "r", Job: "a"}}},
	}))

	if len(graph.Nodes) != 2 {
		t.Errorf("nodes: %d, want 2", len(graph.Nodes))
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

	graph := buildGraph(views)

	byName := map[string]graphNode{}
	for _, node := range graph.Nodes {
		byName[node.Name] = node
	}

	want := map[string][2]string{
		"a": {"passed", "✓"},
		"b": {"never ran", "○"},
		"c": {"failed", "✗"},
		"d": {"running", "◐"},
		"e": {"pending", "○"},
	}

	for name, expect := range want {
		if byName[name].Status != expect[0] || byName[name].Glyph != expect[1] {
			t.Errorf("%s: got %q %q, want %q %q",
				name, byName[name].Glyph, byName[name].Status, expect[1], expect[0])
		}
	}
}

// TestGraphIgnoresAnUnknownUpstream: a passed: naming a job the pipeline does
// not define draws no edge and breaks no layout — the board's table has the
// same tolerance.
func TestGraphIgnoresAnUnknownUpstream(t *testing.T) {
	t.Parallel()

	graph := buildGraph([]jobView{
		{Name: "orphan-dependent", Upstream: []edgeView{{Resource: "r", Job: "no-such-job"}}},
	})

	if len(graph.Nodes) != 1 {
		t.Fatalf("nodes: %d, want 1", len(graph.Nodes))
	}

	if len(graph.Edges) != 0 {
		t.Errorf("edges: %d, want 0 for an unknown upstream", len(graph.Edges))
	}

	if graph.Nodes[0].Layer != 0 {
		t.Errorf("layer: %d, want 0", graph.Nodes[0].Layer)
	}
}

// TestGraphNodeGrowsWithItsName: a long job name widens its node instead of
// overflowing it.
func TestGraphNodeGrowsWithItsName(t *testing.T) {
	t.Parallel()

	// Two graphs, because within one graph a column's nodes share its width.
	short := buildGraph([]jobView{{Name: "a"}}).Nodes[0].W
	long := buildGraph([]jobView{{Name: "a-job-with-a-deliberately-long-name"}}).Nodes[0].W

	if long <= short {
		t.Errorf("long-named node (%dpx) is not wider than the short one (%dpx)", long, short)
	}
}

// TestJobsBoardGraphIsSVG: the toggle's graph view renders real nodes that
// link to job pages, not ASCII art.
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
		`class="dagedge"`,
		`href="/p/demo/jobs/build"`,
		`href="/p/demo/jobs/deploy"`,
		"never ran",
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("graph svg missing %q", want)
		}
	}
}
