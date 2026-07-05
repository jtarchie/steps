package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/journal"
	"github.com/jtarchie/steps/machine"
)

func loadExample(t *testing.T, rel string) *machine.Machine {
	t.Helper()
	m, err := machine.Load(filepath.Join(repoRoot(), rel))
	if err != nil {
		t.Fatalf("load %s: %v", rel, err)
	}
	return m
}

func TestBuildLayoutRanks(t *testing.T) {
	m := loadExample(t, "examples/summarize-critic/workflow.ts")
	lay := buildLayout(m.Graph(), RunOverlay{})

	if lay.w <= 0 || lay.h <= 0 {
		t.Fatalf("layout extent = %.1fx%.1f, want positive", lay.w, lay.h)
	}
	if init := lay.byName["draft"]; init == nil || init.rank != 0 {
		t.Fatalf("initial draft rank = %v, want 0", init)
	}

	sawBack := false
	for _, e := range lay.edges {
		f, to := lay.byName[e.From], lay.byName[e.To]
		if f == nil || to == nil {
			t.Fatalf("edge %s→%s missing an endpoint box", e.From, e.To)
		}
		if e.back {
			sawBack = true
		} else if to.rank <= f.rank {
			t.Errorf("forward edge %s→%s goes rank %d→%d (not increasing)", e.From, e.To, f.rank, to.rank)
		}
		if isNotFinite(f.x) || isNotFinite(f.y) {
			t.Errorf("node %s has non-finite coords %.1f,%.1f", e.From, f.x, f.y)
		}
	}
	if !sawBack {
		t.Error("expected the critique→draft revise loop to be a back edge")
	}
}

func TestRenderGraphSVGStructure(t *testing.T) {
	m := loadExample(t, "examples/summarize-critic/workflow.ts")
	svg := string(renderGraphSVG(m.Graph(), RunOverlay{}))

	for _, want := range []string{
		"<svg", "viewBox=", "<marker", "graph-svg",
		"draft", "critique", "escalate", "publish", "done", "failed",
		`class="node kind-agent`,  // draft/critique
		`class="node kind-human`,  // escalate
		`class="node kind-action`, // publish (write sugar)
		"kind-terminal",           // done/failed
		"when:",                   // the accept/revise guards are labeled
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("static SVG missing %q", want)
		}
	}
	// No run overlaid: no has-run dimming class.
	if strings.Contains(svg, "has-run") {
		t.Error("static SVG should not be marked has-run")
	}
}

func TestRenderGraphSVGOverlay(t *testing.T) {
	m := loadExample(t, "examples/summarize-critic/workflow.ts")
	ov := RunOverlay{
		Visits:  map[string]int{"draft": 2, "critique": 2},
		Fired:   map[[2]string]bool{{"draft", "critique"}: true},
		Current: "critique",
		Status:  "running",
	}
	svg := string(renderGraphSVG(m.Graph(), ov))

	for _, want := range []string{
		"has-run",         // a run is overlaid
		" visited",        // draft/critique visited
		" current",        // critique is current
		" fired",          // the draft→critique edge fired
		"gv-arrow-fired",  // and uses the fired arrowhead
		`data-visits="2"`, // hover data carries the count
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("overlay SVG missing %q", want)
		}
	}
}

// TestRenderGraphSVGEscapes proves guard sources (arbitrary JS) are HTML-escaped
// in edge labels — a guard with < and && must not emit raw markup.
func TestRenderGraphSVGEscapes(t *testing.T) {
	const src = `
const a = { prompt: () => "a", output: { n: "number" } };
const b = { write: "o.md", content: () => "x" };
export default {
  name: "esc",
  model: "mock",
  states: { a, b },
  flow: pipe(branch(a, [ when(({ output }) => output.n < 2 && output.n > 0).to(b), done ])),
};`
	m, err := machine.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	svg := string(renderGraphSVG(m.Graph(), RunOverlay{}))

	if strings.Contains(svg, "output.n < 2") {
		t.Error("guard '<' leaked unescaped into the SVG")
	}
	if !strings.Contains(svg, "&lt;") {
		t.Error("expected the guard '<' to render as &lt;")
	}
	if !strings.Contains(svg, "&amp;") {
		t.Error("expected the guard '&&' to render as &amp;")
	}
}

func TestRenderGraphSVGEmpty(t *testing.T) {
	if got := renderGraphSVG(machine.GraphView{}, RunOverlay{}); got != "" {
		t.Errorf("empty graph = %q, want empty string", got)
	}
}

// isNotFinite reports NaN or ±Inf without importing math into the hot path.
func isNotFinite(f float64) bool { return f != f || f > 1e18 || f < -1e18 }

// TestBuildRunOverlayDistill: journal events carry lowered distill names
// (pred → C#key → C). The overlay must collapse them onto the visible consumer
// so the drawn pred→C edge fires and a mid-distill failure marks C.
func TestBuildRunOverlayDistill(t *testing.T) {
	const src = `
const plan = { prompt: () => "plan", output: { contract: "string" } };
const gen = {
  distill: { contract_slice: { from: "plan", for: "the public contract only" } },
  prompt: ({ contract_slice }) => "gen " + contract_slice,
  output: { code: "string" },
};
export default {
  name: "distilloverlay",
  models: { distiller: "mock" },
  model: "mock",
  states: { plan, gen },
  flow: pipe(plan, gen),
};`
	m, err := machine.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	ev := func(typ journal.EventType, data map[string]any) *journal.Event {
		return &journal.Event{Type: typ, Data: data}
	}
	events := []*journal.Event{
		ev(journal.TransitionFired, map[string]any{"from": "plan", "to": "gen#contract_slice"}),
		ev(journal.HandlerFailed, map[string]any{"state": "gen#contract_slice", "class": "provider_error"}),
		ev(journal.TransitionFired, map[string]any{"from": "gen#contract_slice", "to": "gen"}),
	}
	rs := &journal.RunState{Visits: map[string]int{"plan": 1, "gen#contract_slice": 1, "gen": 1}}
	run := &journal.Run{CurrentState: "gen", Status: journal.StatusRunning}

	ov := buildRunOverlay(m, run, rs, events)

	if !ov.Fired[[2]string{"plan", "gen"}] {
		t.Errorf("Fired should light the collapsed plan→gen edge; got %v", ov.Fired)
	}
	for pair := range ov.Fired {
		if strings.Contains(pair[0], "#") || strings.Contains(pair[1], "#") {
			t.Errorf("Fired leaked a lowered distill name: %v", pair)
		}
	}
	if !ov.Failed["gen"] {
		t.Error("a failure inside gen#contract_slice should mark the consumer gen failed")
	}
	if ov.Failed["gen#contract_slice"] {
		t.Error("Failed should not carry the lowered distill name")
	}
	if ov.Visits["gen"] == 0 {
		t.Error("gen should read as visited (distiller visits folded into the consumer)")
	}
}

// TestOrderRanksReducesCrossings feeds a hand-built layout whose discovery-seed
// ordering crosses (a→d, b→c with a<b, c<d) and asserts the ordering pass
// removes the crossing.
func TestOrderRanksReducesCrossings(t *testing.T) {
	a := &nodeBox{GraphNode: machine.GraphNode{Name: "a"}, rank: 0}
	b := &nodeBox{GraphNode: machine.GraphNode{Name: "b"}, rank: 0}
	c := &nodeBox{GraphNode: machine.GraphNode{Name: "c"}, rank: 1}
	d := &nodeBox{GraphNode: machine.GraphNode{Name: "d"}, rank: 1}
	lay := &gvLayout{
		nodes:  []*nodeBox{a, b, c, d},
		byName: map[string]*nodeBox{"a": a, "b": b, "c": c, "d": d},
		disco:  map[string]int{"a": 0, "b": 1, "c": 2, "d": 3},
	}
	edges := []machine.GraphEdge{{From: "a", To: "d"}, {From: "b", To: "c"}}

	// Seed crossings under discovery order (a<b, c<d): a→d and b→c cross once.
	_, _, fwd := forwardAdjacency(lay, edges)
	seed := map[string]int{"a": 0, "b": 1, "c": 0, "d": 1}
	if got := countCrossings(fwd, lay, seed); got != 1 {
		t.Fatalf("seed crossings = %d, want 1 (the fixture must actually cross)", got)
	}

	byRank := orderRanks(lay, edges, 1)
	final := snapshotOrder(byRank, 1)
	if got := countCrossings(fwd, lay, final); got != 0 {
		t.Errorf("crossings after ordering = %d, want 0 (a→d,b→c should uncross)", got)
	}
}

// TestBuildLayoutDeterministic: identical input yields an identical ordering.
func TestBuildLayoutDeterministic(t *testing.T) {
	m := loadExample(t, "examples/incident-runbook/workflow.ts")
	order := func() map[string]int {
		lay := buildLayout(m.Graph(), RunOverlay{})
		out := map[string]int{}
		for _, nb := range lay.nodes {
			out[nb.Name] = nb.order
		}
		return out
	}
	a, b := order(), order()
	for name, oa := range a {
		if b[name] != oa {
			t.Errorf("ordering not deterministic: %s = %d then %d", name, oa, b[name])
		}
	}
}
