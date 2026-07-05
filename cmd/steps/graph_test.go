package main

import (
	"path/filepath"
	"strings"
	"testing"

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
