package machine

import "testing"

// graphTestMachine exercises every edge kind the diagram must render: a judge
// loop (back edge + catch), a human gate (timeout route), agent/action/human
// handlers, and both terminals.
const graphTestMachine = `
const draft = { prompt: () => "draft the summary", output: { summary: "string" } };
const critique = { prompt: () => "score it", output: { score: "number" }, events: ["approve", "revise"] };
const gate = { human: () => "approve the draft?", timeout: "1h" };
const publish = { write: "out/summary.md", content: () => "done" };
export default {
  name: "graphtest",
  model: "mock",
  states: { draft, critique, gate, publish },
  flow: pipe(
    loop(draft, {
      judge: critique,
      accept: ({ output }) => output.score >= 8,
      maxVisits: 3,
      catch: { provider_error: fail },
      exhausted: branch(gate, { approved: publish, rejected: fail, timeout: fail }),
    }),
    publish,
  ),
};`

func loadGraphTestMachine(t *testing.T) GraphView {
	t.Helper()
	m, err := Parse([]byte(graphTestMachine))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m.Graph()
}

func TestGraphNodes(t *testing.T) {
	g := loadGraphTestMachine(t)

	byName := map[string]GraphNode{}
	for _, n := range g.Nodes {
		byName[n.Name] = n
	}
	for name, kind := range map[string]string{
		"draft": "agent", "critique": "agent", "gate": "human",
		"publish": "action", "done": "terminal", "failed": "terminal",
	} {
		n, ok := byName[name]
		if !ok {
			t.Fatalf("missing node %q; have %v", name, byName)
		}
		if n.Kind != kind {
			t.Errorf("node %q kind = %q, want %q", name, n.Kind, kind)
		}
	}

	if g.Initial != "draft" || !byName["draft"].Initial {
		t.Errorf("initial = %q (draft.Initial=%v), want draft flagged", g.Initial, byName["draft"].Initial)
	}
	initials := 0
	for _, n := range g.Nodes {
		if n.Initial {
			initials++
		}
	}
	if initials != 1 {
		t.Errorf("initial-flagged nodes = %d, want exactly 1", initials)
	}

	if f := byName["failed"]; !f.Terminal || f.Status != "failed" {
		t.Errorf("failed node = %+v, want terminal with status failed", f)
	}
	if d := byName["done"]; !d.Terminal || d.Status != "" {
		t.Errorf("done node = %+v, want terminal success", d)
	}
	if m := byName["draft"].Model; m != "mock" {
		t.Errorf("draft model = %q, want mock", m)
	}
}

func TestGraphEdges(t *testing.T) {
	g := loadGraphTestMachine(t)

	has := func(from, to string, kind EdgeKind) bool {
		for _, e := range g.Edges {
			if e.From == from && e.To == to && e.Kind == kind {
				return true
			}
		}
		return false
	}

	want := []struct {
		from, to string
		kind     EdgeKind
		desc     string
	}{
		{"critique", "draft", EdgeNormal, "the revise back edge"},
		{"critique", "publish", EdgeNormal, "the accept exit"},
		{"critique", "gate", EdgeFallback, "the exhausted route into the gate"},
		{"critique", "failed", EdgeCatch, "the loop catch lowered onto the judge"},
		{"gate", "publish", EdgeNormal, "the gate approved route"},
		{"gate", "failed", EdgeTimeout, "the gate timeout route"},
		{"publish", "done", EdgeFallback, "the pipe tail fallthrough"},
	}
	for _, w := range want {
		if !has(w.from, w.to, w.kind) {
			t.Errorf("missing %s (%s→%s %s)", w.desc, w.from, w.to, w.kind)
		}
	}

	// Every edge endpoint must be a real (visible) node.
	names := map[string]bool{}
	for _, n := range g.Nodes {
		names[n.Name] = true
	}
	for _, e := range g.Edges {
		if !names[e.From] || !names[e.To] {
			t.Errorf("edge %s→%s has an endpoint absent from nodes", e.From, e.To)
		}
	}
}

// TestGraphHidesDistill verifies implicit distill states are collapsed: the
// predecessor's edge lands on the consumer, and no distill node survives.
func TestGraphHidesDistill(t *testing.T) {
	const src = `
const plan = { prompt: () => "plan", output: { contract: "string" } };
const gen = {
  distill: { contract_slice: { from: "plan", for: "the public contract only" } },
  prompt: ({ contract_slice }) => "gen " + contract_slice,
  output: { code: "string" },
};
export default {
  name: "distilltest",
  models: { distiller: "mock" },
  model: "mock",
  states: { plan, gen },
  flow: pipe(plan, gen),
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Sanity: the lowering really did create a hidden distill state.
	if m.State("gen#contract_slice") == nil {
		t.Fatal("expected implicit distill state gen#contract_slice")
	}
	g := m.Graph()

	for _, n := range g.Nodes {
		if s := m.State(n.Name); s != nil && s.IsDistill() {
			t.Errorf("distill node %q leaked into the graph", n.Name)
		}
	}
	// plan must connect straight to gen, not to the hidden distill chain head.
	found := false
	for _, e := range g.Edges {
		if e.From == "plan" && e.To == "gen" {
			found = true
		}
		if s := m.State(e.To); s != nil && s.IsDistill() {
			t.Errorf("edge %s→%s points at a hidden distill node", e.From, e.To)
		}
	}
	if !found {
		t.Error("plan→gen edge missing after distill collapse")
	}

	// VisibleState maps the lowered distill hop onto its consumer, and leaves
	// ordinary states untouched — this is what the run overlay uses to project
	// journal events (which carry lowered names) onto the drawn nodes.
	if got := m.VisibleState("gen#contract_slice"); got != "gen" {
		t.Errorf("VisibleState(gen#contract_slice) = %q, want gen", got)
	}
	if got := m.VisibleState("plan"); got != "plan" {
		t.Errorf("VisibleState(plan) = %q, want plan", got)
	}
	if got := m.VisibleState("nonexistent"); got != "nonexistent" {
		t.Errorf("VisibleState(nonexistent) = %q, want it unchanged", got)
	}
}
